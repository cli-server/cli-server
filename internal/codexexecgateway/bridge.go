package codexexecgateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/relaypb"
	"github.com/agentserver/agentserver/internal/wsbridge"
	"github.com/go-chi/chi/v5"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

// handleBridge accepts a ws connection from an env-mcp child binary
// and registers it as a routed stream on the executor's inboundConn.
// Multiple concurrent /bridge sessions for the same exe_id share the
// inbound, demultiplexed by relay stream_id (per the 2026-05-17 spec).
//
// HTTP error codes:
//
//	400 — first ws frame missing/malformed/not a Resume
//	401 — bad/expired cap token, or revoked turn_id
//	403 — workspace doesn't own the URL exe_id
//	503 — exe_id has no live inbound connection
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	exeID := chi.URLParam(r, "exe_id")
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authz, "Bearer ")
	if exeID == "" || token == "" {
		http.Error(w, "missing parameters", http.StatusBadRequest)
		return
	}

	// 1. Verify cap token (signature + TTL) BEFORE registry lookup.
	payload, err := VerifyCapabilityToken(token, s.config.CapTokenHMACSecret)
	if err != nil {
		s.logger.Warn("bridge: auth failed", "exe_id", exeID, "error", err, "remote", r.RemoteAddr)
		switch {
		case errors.Is(err, ErrExpired):
			http.Error(w, "token expired", http.StatusUnauthorized)
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}

	// 2. Revocation check (in-memory; cheaper than the DB owns check).
	if s.revoked.Contains(payload.TurnID) {
		s.logger.Warn("bridge: rejected revoked turn", "exe_id", exeID, "turn_id", payload.TurnID)
		http.Error(w, "turn revoked", http.StatusUnauthorized)
		return
	}

	// 3. Workspace ownership.
	if s.store == nil {
		s.logger.Warn("bridge: skipping ownership check — store is nil (test wiring)")
	} else {
		ownsCtx, ownsCancel := context.WithTimeout(r.Context(), 2*time.Second)
		owns, err := s.store.OwnsExecutor(ownsCtx, payload.WorkspaceID, exeID)
		ownsCancel()
		if err != nil {
			s.logger.Error("bridge: ownership check failed",
				"workspace_id", payload.WorkspaceID, "exe_id", exeID, "error", err)
			http.Error(w, "ownership check failed", http.StatusInternalServerError)
			return
		}
		if !owns {
			s.logger.Warn("bridge: forbidden",
				"workspace_id", payload.WorkspaceID, "exe_id", exeID,
				"reason", "exe_id_not_in_workspace", "turn_id", payload.TurnID)
			http.Error(w, "exe_id not in workspace", http.StatusForbidden)
			return
		}
	}

	// 4. Look up inbound. 503 if none.
	inbound, ok := s.registry.Lookup(exeID)
	if !ok {
		s.logger.Warn("bridge: no inbound conn", "exe_id", exeID, "turn_id", payload.TurnID)
		http.Error(w, "executor not connected", http.StatusServiceUnavailable)
		return
	}

	// 4b. Per-executor stream cap (PR 1 Gap 3): Strategy A — pre-upgrade HTTP 503.
	// We do the check before websocket.Accept to fail-fast on capacity-exceeded
	// dials. The check is intentionally not atomic with the eventual addRoute;
	// concurrent dials slipping past will succeed at addRoute. Acceptable: the
	// cap is a guardrail, not a strict invariant. Bridge dials are typically
	// sequential per env-mcp. cap=0 means disabled (no enforcement).
	if cap := s.config.MaxStreamsPerExecutor; cap > 0 && inbound.streamCount() >= cap {
		s.logger.Warn("bridge: stream cap exceeded",
			"exe_id", exeID, "cap", cap, "current", inbound.streamCount())
		w.Header().Set("Retry-After", "30")
		http.Error(w, "executor stream cap exceeded", http.StatusServiceUnavailable)
		return
	}

	// 5. Upgrade caller to ws.
	bridgeWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // auth already enforced above
	})
	if err != nil {
		s.logger.Error("bridge: ws accept", "exe_id", exeID, "error", err)
		return
	}
	bridgeWS.SetReadLimit(s.config.MaxFrameBytes)

	// 6. Peek the Resume frame — env-mcp's BridgeClient always sends it
	// first on dial; that's where we learn the stream_id for routing.
	mt, first, err := bridgeWS.Read(r.Context())
	if err != nil {
		s.logger.Warn("bridge: read first frame failed", "exe_id", exeID, "error", err)
		_ = bridgeWS.Close(websocket.StatusProtocolError, "first frame read failed")
		return
	}
	if mt != websocket.MessageBinary {
		s.logger.Warn("bridge: first frame not binary", "exe_id", exeID, "type", mt.String())
		_ = bridgeWS.Close(websocket.StatusProtocolError, "first frame must be binary Resume")
		return
	}
	var firstFrame relaypb.RelayMessageFrame
	if err := proto.Unmarshal(first, &firstFrame); err != nil {
		s.logger.Warn("bridge: first frame parse failed", "exe_id", exeID, "error", err)
		_ = bridgeWS.Close(websocket.StatusProtocolError, "malformed first frame")
		return
	}
	if _, isResume := firstFrame.Body.(*relaypb.RelayMessageFrame_Resume); !isResume {
		s.logger.Warn("bridge: first frame not Resume", "exe_id", exeID)
		_ = bridgeWS.Close(websocket.StatusProtocolError, "first frame must be Resume")
		return
	}
	streamID := firstFrame.StreamId
	if streamID == "" {
		s.logger.Warn("bridge: Resume missing stream_id", "exe_id", exeID)
		_ = bridgeWS.Close(websocket.StatusProtocolError, "Resume missing stream_id")
		return
	}

	// 7. Register the route. UUID collision is astronomical, but we
	// evict the prior session defensively (and log loudly) if it ever
	// happens — better than silently swapping who receives the next
	// frame.
	session := newBridgeSession(streamID, inbound, bridgeWS)
	if evicted := inbound.addRoute(streamID, session); evicted != nil {
		s.logger.Warn("bridge: stream_id collision; evicting prior session",
			"exe_id", exeID, "stream_id", streamID)
		evicted.close(errors.New("evicted by stream_id collision"))
	}
	defer func() {
		inbound.removeRoute(streamID, session)
		session.close(nil)
	}()

	s.logger.Info("bridge: paired", "exe_id", exeID, "stream_id", streamID, "turn_id", payload.TurnID)

	// 8. Forward the Resume frame to inbound so exec-server's relay
	// layer learns about this new stream.
	if err := inbound.write(r.Context(), websocket.MessageBinary, first); err != nil {
		s.logger.Warn("bridge: forward Resume failed", "exe_id", exeID, "error", err)
		return
	}

	// 9. Keep-alive on the bridge ws (same as inbound — defensive
	// against middlebox idle kills between codex tool calls).
	keepAliveCtx, stopKeepAlive := context.WithCancel(r.Context())
	defer stopKeepAlive()
	go wsbridge.KeepAlive(keepAliveCtx, bridgeWS, 30*time.Second)

	// 10. Pump bridge → inbound until either side closes.
	s.runBridgePump(r.Context(), session)
}

// runBridgePump reads frames from session.bridgeWS and writes them to
// inbound under inbound's writeMu. Drops frames whose stream_id
// doesn't match the session's (defends against env-mcp bugs). Returns
// when either side closes.
func (s *Server) runBridgePump(ctx context.Context, session *bridgeSession) {
	for {
		select {
		case <-session.closed:
			return
		case <-session.inbound.closed:
			return
		case <-ctx.Done():
			return
		default:
		}
		mt, data, err := session.bridgeWS.Read(ctx)
		if err != nil {
			closeStatus := websocket.CloseStatus(err)
			if closeStatus != websocket.StatusNormalClosure && closeStatus != websocket.StatusGoingAway {
				s.logger.Info("bridge: pump ended", "stream_id", session.streamID, "error", err)
			}
			return
		}
		// Mark activity on every read from the bridge, even non-binary or
		// wrong-stream_id ones — the connection IS being used.
		session.touch()
		if mt != websocket.MessageBinary {
			continue
		}
		// Validate stream_id matches session's. If mismatched, drop —
		// keeps the inbound's frame ordering coherent.
		var f relaypb.RelayMessageFrame
		if proto.Unmarshal(data, &f) == nil && f.StreamId != session.streamID {
			s.logger.Warn("bridge: ignoring frame with wrong stream_id",
				"want", session.streamID, "got", f.StreamId)
			continue
		}
		if err := session.inbound.write(ctx, mt, data); err != nil {
			s.logger.Warn("bridge: inbound write failed", "stream_id", session.streamID, "error", err)
			return
		}
	}
}
