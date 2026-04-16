package ccbroker

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceID string  `json:"workspace_id"`
		Title       string  `json:"title"`
		Source      string  `json:"source"`
		ExternalID  *string `json:"external_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.WorkspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}

	if req.Source == "" {
		req.Source = "stateless_cc"
	}

	sessionID := "cse_" + uuid.NewString()

	if err := s.store.CreateSession(r.Context(), sessionID, req.WorkspaceID, req.Title, req.Source, req.ExternalID); err != nil {
		s.logger.Error("create session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"session": map[string]string{
			"id": sessionID,
		},
	})
}
