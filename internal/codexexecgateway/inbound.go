package codexexecgateway

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
	"nhooyr.io/websocket"
)

// handleInbound accepts the long-lived ws connection from a local
// `codex exec-server --connect` process. The token is supplied as a query
// string parameter so the codex-exec --auth-token-env flow works without
// custom headers.
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
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Error("inbound: ws accept", "exe_id", exeID, "error", err)
		return
	}
	ws.SetReadLimit(-1) // codex exec-server streams large process/read responses

	if evicted := s.registry.Register(exeID, ws); evicted != nil {
		s.logger.Info("inbound: evicted prior conn", "exe_id", exeID)
		evicted.Close(websocket.StatusPolicyViolation, "replaced by new connection")
	}
	if err := s.store.UpdateLastSeen(r.Context(), exeID); err != nil {
		s.logger.Warn("inbound: update last_seen", "exe_id", exeID, "error", err)
	}
	s.logger.Info("inbound: connected", "exe_id", exeID)

	// Block until the client disconnects or the bridge pump closes the conn.
	// We do not parse frames here — the bridge pump in /bridge/{exe_id}
	// will read from this conn while it is paired. While unpaired, we just
	// hold the conn open and respond to keepalive pings (handled by nhooyr).
	<-r.Context().Done()
	_ = ws.Close(websocket.StatusNormalClosure, "")
	s.registry.Unregister(exeID, ws)
	bg := context.Background()
	if err := s.store.UpdateLastSeen(bg, exeID); err != nil {
		s.logger.Warn("inbound: final last_seen", "exe_id", exeID, "error", err)
	}
	s.logger.Info("inbound: disconnected", "exe_id", exeID)
}
