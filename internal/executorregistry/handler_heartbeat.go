package executorregistry

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

type heartbeatRequest struct {
	Status       string              `json:"status"`
	SystemInfo   json.RawMessage     `json:"system_info"`
	Capabilities *ExecutorCapability `json:"capabilities,omitempty"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	executorID := chi.URLParam(r, "id")

	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "missing or invalid authorization header")
		return
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")

	valid, err := s.store.ValidateRegistryToken(r.Context(), executorID, token)
	if err != nil {
		s.logger.Error("validate registry token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !valid {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	var req heartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Status != "" {
		if err := s.store.UpdateExecutorStatus(r.Context(), executorID, req.Status); err != nil {
			s.logger.Error("update executor status", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if err := s.store.UpdateHeartbeat(r.Context(), executorID, req.SystemInfo); err != nil {
		s.logger.Error("update heartbeat", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if req.Capabilities != nil {
		req.Capabilities.ExecutorID = executorID
		if err := s.store.UpdateCapabilities(r.Context(), executorID, *req.Capabilities); err != nil {
			s.logger.Error("update capabilities via heartbeat", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
