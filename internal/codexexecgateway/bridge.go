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
		http.Error(w, "exe_id not in token allow set", http.StatusForbidden)
		return
	}

	// 3. Check revocation (after token is valid so we don't prematurely leak).
	if s.revoked.Contains(payload.TurnID) {
		http.Error(w, "turn revoked", http.StatusUnauthorized)
		return
	}

	// 4. Look up registered inbound conn.
	inbound, ok := s.registry.Lookup(exeID)
	if !ok {
		http.Error(w, "executor not connected", http.StatusServiceUnavailable)
		return
	}

	// 5. Upgrade caller to ws.
	bridge, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Error("bridge: ws accept", "exe_id", exeID, "error", err)
		return
	}
	s.logger.Info("bridge: paired", "exe_id", exeID, "turn_id", payload.TurnID)

	// 6. Run paired frame pumps. Cancel propagates to both pumps so the
	// second one exits when the first returns.
	pumpCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 2)
	go func() { errCh <- pumpFrames(pumpCtx, bridge, inbound) }()
	go func() { errCh <- pumpFrames(pumpCtx, inbound, bridge) }()

	// Wait for either pump to return; cancel so the other pump unblocks.
	first := <-errCh
	cancel()
	// Close the bridge side; inbound side is intentionally left open so the
	// executor conn can be re-paired by a subsequent /bridge/{exe_id} request.
	bridge.Close(websocket.StatusNormalClosure, "peer closed")
	<-errCh // drain second pump

	if first != nil {
		s.logger.Info("bridge: pump ended", "exe_id", exeID, "err", first)
	} else {
		s.logger.Info("bridge: pump ended", "exe_id", exeID)
	}
}
