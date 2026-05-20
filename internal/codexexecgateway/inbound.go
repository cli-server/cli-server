package codexexecgateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/relaypb"
	"github.com/agentserver/agentserver/internal/wsbridge"
	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

// handleInbound accepts the long-lived ws connection from a local
// `codex exec-server --remote` process and runs the relay frame
// reader loop on it. The reader demultiplexes incoming frames by
// stream_id to per-session bridge connections registered via
// /bridge/{exe_id} (per the 2026-05-17 multiplexing spec).
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	exeID := chi.URLParam(r, "exe_id")
	token := r.URL.Query().Get("token")
	if exeID == "" || token == "" {
		http.Error(w, "missing parameters", http.StatusBadRequest)
		return
	}

	hash, err := s.store.GetRegistrationTokenHash(r.Context(), exeID)
	if err != nil {
		s.logger.Error("inbound: get token hash", "exe_id", exeID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hash == "" {
		slog.Warn("inbound: unauthorized", "exe_id", exeID, "reason", "unknown_exe_id", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)); err != nil {
		slog.Warn("inbound: unauthorized", "exe_id", exeID, "reason", "bad_token", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // auth enforced by bcrypt check above
	})
	if err != nil {
		s.logger.Error("inbound: ws accept", "exe_id", exeID, "error", err)
		return
	}
	ic := newInboundConn(exeID, ws, s.logger.With("exe_id", exeID), s.config.MaxFrameBytes)
	if evicted := s.registry.Register(exeID, ic); evicted != nil {
		s.logger.Info("inbound: evicted prior conn", "exe_id", exeID)
		evicted.close(nil)
	}
	if err := s.store.UpdateLastSeen(r.Context(), exeID); err != nil {
		s.logger.Warn("inbound: update last_seen", "exe_id", exeID, "error", err)
	}
	s.logger.Info("inbound: connected", "exe_id", exeID)

	// 30s ws PING (control frame) is layered on TCP keepalive (15s) for
	// middlebox idle-kill resistance.
	keepAliveCtx, stopKeepAlive := context.WithCancel(r.Context())
	defer stopKeepAlive()
	go wsbridge.KeepAlive(keepAliveCtx, ws, 30*time.Second)

	// Idle bridge reaper: per-stream timeout enforcement. Closes silent
	// /bridge sessions after BridgeIdleTimeout and emits RelayReset to
	// the executor's relay layer. Cancelled by r.Context() (ws close).
	go ic.startIdleReaper(r.Context(), s.config.BridgeIdleTimeout)

	// Reader loop: parse each binary frame as a RelayMessageFrame,
	// route Data/Reset by stream_id, drop Ack/Resume/Heartbeat (those
	// are rendezvous-internal per the upstream relay spec — exec-server
	// and harness handle them; rendezvous "only routes frames").
	s.runInboundReader(r.Context(), ic)

	s.registry.Unregister(exeID, ic)
	ic.close(nil)
	bg := context.Background()
	if err := s.store.UpdateLastSeen(bg, exeID); err != nil {
		s.logger.Warn("inbound: final last_seen", "exe_id", exeID, "error", err)
	}
	s.logger.Info("inbound: disconnected", "exe_id", exeID)
}

// runInboundReader drains ic.ws, decoding each binary frame as a
// RelayMessageFrame and dispatching by stream_id to the registered
// bridge session. Returns when the conn closes.
func (s *Server) runInboundReader(ctx context.Context, ic *inboundConn) {
	for {
		mt, data, err := ic.ws.Read(ctx)
		if err != nil {
			ic.logger.Info("inbound: read ended", "error", err)
			return
		}
		if mt != websocket.MessageBinary {
			ic.logger.Warn("inbound: ignoring non-binary frame", "type", mt.String())
			continue
		}
		var frame relaypb.RelayMessageFrame
		if err := proto.Unmarshal(data, &frame); err != nil {
			ic.logger.Warn("inbound: malformed relay frame", "error", err)
			continue
		}
		if frame.StreamId == "" {
			ic.logger.Warn("inbound: relay frame missing stream_id")
			continue
		}
		b, ok := ic.lookup(frame.StreamId)
		if !ok {
			// Could be a race: bridge just unregistered. Drop quietly at
			// debug — exec-server's relay layer will eventually time out
			// the orphaned stream.
			ic.logger.Debug("inbound: no route for stream", "stream_id", frame.StreamId)
			continue
		}
		// Mark activity BEFORE the (potentially blocking) bridge write so
		// the reaper doesn't false-positive a session that's actively
		// pumping but momentarily slow.
		b.touch()
		if err := b.write(ctx, mt, data); err != nil {
			ic.logger.Warn("inbound: bridge write failed; closing route", "stream_id", frame.StreamId, "error", err)
			b.close(err)
			ic.removeRoute(frame.StreamId, b)
			continue
		}
		// On Reset, drop the route after forwarding (exec-server's relay
		// signalled end-of-stream; the bridge ws will close shortly).
		if _, isReset := frame.Body.(*relaypb.RelayMessageFrame_Reset_); isReset {
			ic.removeRoute(frame.StreamId, b)
		}
	}
}
