package codexexecgateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"nhooyr.io/websocket"
)

// handleBridge accepts a ws connection from an env-mcp child binary
// (codex-app-gateway env-mcp ...) and pairs it with the registered
// inbound /codex-exec/{exe_id} conn. Auth is verified once at connect
// time (cap-token verify BEFORE registry lookup so unauthenticated callers
// don't learn which exe_ids exist); thereafter forwarding is unconditional
// until either side closes.
//
// HTTP error codes:
//   401 — bad/expired cap token, or revoked turn_id
//   403 — URL exe_id not in token allow-list
//   503 — exe_id not in registry (no inbound connection)
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	exeID := chi.URLParam(r, "exe_id")
	token := r.URL.Query().Get("token")
	if exeID == "" || token == "" {
		http.Error(w, "missing parameters", http.StatusBadRequest)
		return
	}

	// 1. Verify cap token BEFORE registry lookup to prevent exe_id enumeration.
	payload, err := VerifyCapabilityToken(token, s.config.CapTokenHMACSecret)
	if err != nil {
		s.logger.Warn("bridge: auth failed", "exe_id", exeID, "error", err, "remote", r.RemoteAddr)
		switch {
		case errors.Is(err, ErrExpired):
			http.Error(w, "token expired", http.StatusUnauthorized)
		case errors.Is(err, ErrBadSignature), errors.Is(err, ErrMalformed):
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}

	// 2. Check URL exe_id is in the token's allow-list.
	if !payload.AllowsExeID(exeID) {
		s.logger.Warn("bridge: forbidden", "exe_id", exeID, "reason", "exe_id_not_in_token_allow_list", "turn_id", payload.TurnID)
		http.Error(w, "exe_id not in token allow set", http.StatusForbidden)
		return
	}

	// 3. Check revocation — TurnID is only available from the decoded payload, so this must come after signature verification.
	if s.revoked.Contains(payload.TurnID) {
		s.logger.Warn("bridge: rejected revoked turn", "exe_id", exeID, "turn_id", payload.TurnID)
		http.Error(w, "turn revoked", http.StatusUnauthorized)
		return
	}

	// 4. Look up registered inbound conn.
	inbound, ok := s.registry.Lookup(exeID)
	if !ok {
		s.logger.Warn("bridge: no inbound conn", "exe_id", exeID, "turn_id", payload.TurnID)
		http.Error(w, "executor not connected", http.StatusServiceUnavailable)
		return
	}

	// 5. Upgrade caller to ws.
	bridge, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // skip HTTP Origin check; auth is enforced by token verification above
	})
	if err != nil {
		s.logger.Error("bridge: ws accept", "exe_id", exeID, "error", err)
		return
	}
	bridge.SetReadLimit(-1) // codex exec-server streams large process/read responses
	s.logger.Info("bridge: paired", "exe_id", exeID, "turn_id", payload.TurnID)

	// 6. Run paired frame pumps. Cancel propagates to both pumps so the
	// second one exits when the first returns. Derived from r.Context() so
	// graceful shutdown (httpServer.Shutdown) drains active sessions instead
	// of leaking pump goroutines.
	pumpCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	errCh := make(chan error, 2)
	go func() { errCh <- pumpFrames(pumpCtx, bridge, inbound) }()
	go func() { errCh <- pumpFrames(pumpCtx, inbound, bridge) }()

	// Wait for either pump to return; cancel so the other pump unblocks.
	first := <-errCh
	cancel()
	// Close the bridge side; inbound side is intentionally left open so the
	// executor conn can be re-paired by a subsequent /bridge/{exe_id} request.
	if err := bridge.Close(websocket.StatusNormalClosure, "peer closed"); err != nil {
		s.logger.Warn("bridge: close bridge conn", "exe_id", exeID, "error", err)
	}
	if second := <-errCh; second != nil {
		s.logger.Warn("bridge: second pump ended with error", "exe_id", exeID, "error", second)
	}

	if first != nil {
		s.logger.Warn("bridge: pump ended with error", "exe_id", exeID, "error", first)
	} else {
		s.logger.Info("bridge: pump ended cleanly", "exe_id", exeID)
	}
}
