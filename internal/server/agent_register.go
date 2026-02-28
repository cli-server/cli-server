package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/imryao/cli-server/internal/auth"
)

// handleCreateAgentCode generates a one-time registration code for connecting a local agent.
func (s *Server) handleCreateAgentCode(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wid")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer", "developer") {
		return
	}

	userID := auth.UserIDFromContext(r.Context())

	// Generate a random 12-byte code (24 hex chars).
	codeBytes := make([]byte, 12)
	if _, err := rand.Read(codeBytes); err != nil {
		http.Error(w, "failed to generate code", http.StatusInternalServerError)
		return
	}
	code := hex.EncodeToString(codeBytes)
	expiresAt := time.Now().Add(10 * time.Minute)

	if err := s.DB.CreateAgentRegistrationCode(code, userID, wsID, expiresAt); err != nil {
		log.Printf("failed to create agent registration code: %v", err)
		http.Error(w, "failed to create code", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"code":      code,
		"expiresAt": expiresAt.Format(time.RFC3339),
	})
}

// handleAgentRegister processes a cli-agent registration using a one-time code.
func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		req.Name = "Local Agent"
	}

	// Atomically consume the registration code.
	arc, err := s.DB.ConsumeAgentRegistrationCode(req.Code)
	if err != nil {
		log.Printf("failed to consume agent registration code: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if arc == nil {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}

	sandboxID := uuid.New().String()
	tunnelToken := generatePassword()
	opencodePassword := generatePassword()
	proxyToken := generatePassword()

	if err := s.DB.CreateLocalSandbox(sandboxID, arc.WorkspaceID, req.Name, "opencode", opencodePassword, proxyToken, tunnelToken); err != nil {
		log.Printf("failed to create local sandbox: %v", err)
		http.Error(w, "failed to register agent", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"sandboxId":   sandboxID,
		"tunnelToken": tunnelToken,
		"workspaceId": arc.WorkspaceID,
	})
}
