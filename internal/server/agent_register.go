package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/agentserver/agentserver/internal/shortid"
)

// handleAgentRegister processes a CLI agent registration using an OAuth Bearer token.
// The token must contain workspace_id and agent:register scope from the Hydra consent flow.
func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	// Extract Bearer token.
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		http.Error(w, "missing or invalid Authorization header", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")

	// Introspect token via Hydra.
	if s.HydraClient == nil {
		http.Error(w, "OAuth not configured", http.StatusServiceUnavailable)
		return
	}
	introspection, err := s.HydraClient.IntrospectToken(token)
	if err != nil {
		log.Printf("agent register: introspect token: %v", err)
		http.Error(w, "token introspection failed", http.StatusInternalServerError)
		return
	}
	if !introspection.Active {
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}
	if !introspection.HasScope("agent:register") {
		http.Error(w, "insufficient scope: agent:register required", http.StatusForbidden)
		return
	}

	// Extract workspace_id from token claims.
	workspaceID, _ := introspection.Extra["workspace_id"].(string)
	if workspaceID == "" {
		http.Error(w, "token missing workspace_id claim", http.StatusBadRequest)
		return
	}
	userID := introspection.Subject

	// Verify workspace membership (defense in depth).
	role, err := s.DB.GetWorkspaceMemberRole(workspaceID, userID)
	if err != nil {
		log.Printf("agent register: check role: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if role == "" || role == "guest" {
		http.Error(w, "no permission to register agent in this workspace", http.StatusForbidden)
		return
	}

	// Parse request body.
	var req struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		req.Name = "Local Agent"
	}
	sandboxType := req.Type
	if sandboxType == "" {
		sandboxType = "opencode"
	}
	if sandboxType != "opencode" && sandboxType != "claudecode" {
		http.Error(w, "invalid type: must be opencode or claudecode", http.StatusBadRequest)
		return
	}

	// Create sandbox.
	sandboxID := uuid.New().String()
	tunnelToken := generatePassword()
	proxyToken := generatePassword()
	var opencodePassword string
	if sandboxType == "opencode" {
		opencodePassword = generatePassword()
	}

	sid := shortid.Generate()
	var createErr error
	for attempts := 0; attempts < 3; attempts++ {
		createErr = s.DB.CreateLocalSandbox(sandboxID, workspaceID, req.Name, sandboxType, opencodePassword, proxyToken, tunnelToken, sid)
		if createErr == nil {
			break
		}
		sid = shortid.Generate()
	}
	if createErr != nil {
		log.Printf("agent register: create sandbox: %v", createErr)
		http.Error(w, "failed to register agent", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{
		"sandbox_id":   sandboxID,
		"tunnel_token": tunnelToken,
		"proxy_token":  proxyToken,
		"workspace_id": workspaceID,
		"short_id":     sid,
	})
}
