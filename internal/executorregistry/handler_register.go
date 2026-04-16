package executorregistry

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type registerRequest struct {
	Name        string `json:"name"`
	WorkspaceID string `json:"workspace_id"`
}

type registerResponse struct {
	ExecutorID    string `json:"executor_id"`
	TunnelToken   string `json:"tunnel_token"`
	RegistryToken string `json:"registry_token"`
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func generateExecutorID() string {
	return "exe_" + uuid.NewString()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.TrimSpace(req.WorkspaceID) == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}

	tunnelToken, err := generateToken()
	if err != nil {
		s.logger.Error("generate tunnel token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	registryToken, err := generateToken()
	if err != nil {
		s.logger.Error("generate registry token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC()
	executor := Executor{
		ID:          generateExecutorID(),
		WorkspaceID: req.WorkspaceID,
		Name:        req.Name,
		Type:        "local_agent",
		Status:      "online",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.store.CreateExecutor(r.Context(), executor, tunnelToken, registryToken); err != nil {
		s.logger.Error("create executor", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, registerResponse{
		ExecutorID:    executor.ID,
		TunnelToken:   tunnelToken,
		RegistryToken: registryToken,
	})
}
