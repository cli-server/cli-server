package executorregistry

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type registerSandboxRequest struct {
	SandboxID    string             `json:"sandbox_id"`
	WorkspaceID  string             `json:"workspace_id"`
	Name         string             `json:"name"`
	Capabilities *ExecutorCapability `json:"capabilities,omitempty"`
}

func (s *Server) handleRegisterSandbox(w http.ResponseWriter, r *http.Request) {
	var req registerSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.SandboxID) == "" {
		writeError(w, http.StatusBadRequest, "sandbox_id is required")
		return
	}
	if strings.TrimSpace(req.WorkspaceID) == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	now := time.Now().UTC()
	executor := Executor{
		ID:          req.SandboxID,
		WorkspaceID: req.WorkspaceID,
		Name:        req.Name,
		Type:        "sandbox",
		Status:      "online",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.store.CreateExecutor(r.Context(), executor, "", ""); err != nil {
		s.logger.Error("create sandbox executor", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if req.Capabilities != nil {
		cap := *req.Capabilities
		cap.ExecutorID = executor.ID
		if err := s.store.UpdateCapabilities(r.Context(), executor.ID, cap); err != nil {
			s.logger.Error("update sandbox capabilities", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"executor_id": executor.ID,
		"status":      executor.Status,
	})
}
