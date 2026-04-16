package ccbroker

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")

	// 1. Get session, return 404 if not found
	session, err := s.store.GetSession(r.Context(), sessionID)
	if err != nil {
		s.logger.Error("get session failed", "error", err, "session_id", sessionID)
		writeError(w, http.StatusInternalServerError, "failed to get session")
		return
	}
	if session == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	// 2. Bump epoch atomically
	newEpoch, err := s.store.BumpSessionEpoch(r.Context(), sessionID)
	if err != nil {
		s.logger.Error("bump session epoch failed", "error", err, "session_id", sessionID)
		writeError(w, http.StatusInternalServerError, "failed to bump session epoch")
		return
	}

	// 3. Upsert worker record
	if err := s.store.UpsertWorker(r.Context(), sessionID, newEpoch); err != nil {
		s.logger.Error("upsert worker failed", "error", err, "session_id", sessionID)
		writeError(w, http.StatusInternalServerError, "failed to register worker")
		return
	}

	// 4. Issue JWT
	claims := WorkerJWTClaims{
		SessionID:   sessionID,
		WorkspaceID: session.WorkspaceID,
		Epoch:       newEpoch,
	}
	jwt, err := IssueWorkerJWT(s.config.JWTSecret, claims)
	if err != nil {
		s.logger.Error("issue worker jwt failed", "error", err, "session_id", sessionID)
		writeError(w, http.StatusInternalServerError, "failed to issue worker JWT")
		return
	}

	// 5. Build api_base_url
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd == "https" || fwd == "http" {
		scheme = fwd
	}
	apiBaseURL := fmt.Sprintf("%s://%s/v1/sessions/%s", scheme, r.Host, sessionID)

	// 6. Return BridgeResponse
	writeJSON(w, http.StatusOK, BridgeResponse{
		WorkerJWT:   jwt,
		APIBaseURL:  apiBaseURL,
		ExpiresIn:   86400,
		WorkerEpoch: newEpoch,
	})
}
