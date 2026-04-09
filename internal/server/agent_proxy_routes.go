package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/agentserver/agentserver/internal/db"
)

// extractProxyTokenSandbox validates a Bearer proxy_token and returns the sandbox.
// Returns nil and writes an error response if auth fails.
func (s *Server) extractProxyTokenSandbox(w http.ResponseWriter, r *http.Request) *db.Sandbox {
	token := r.Header.Get("Authorization")
	if len(token) > 7 && token[:7] == "Bearer " {
		token = token[7:]
	} else {
		http.Error(w, "missing authorization", http.StatusUnauthorized)
		return nil
	}
	sbx, err := s.DB.GetSandboxByAnyToken(token)
	if err != nil || sbx == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil
	}
	return sbx
}

// handleAgentDiscoverAgents is the proxy_token-authed version of handleListAgentCards.
// GET /api/agent/discovery/agents
func (s *Server) handleAgentDiscoverAgents(w http.ResponseWriter, r *http.Request) {
	sbx := s.extractProxyTokenSandbox(w, r)
	if sbx == nil {
		return
	}
	cards, err := s.DB.ListAgentCardsByWorkspace(sbx.WorkspaceID)
	if err != nil {
		log.Printf("agent discover: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if cards == nil {
		cards = []db.AgentCard{}
	}

	type cardResponse struct {
		AgentID     string          `json:"agent_id"`
		DisplayName string          `json:"display_name"`
		Description string          `json:"description"`
		AgentType   string          `json:"agent_type"`
		Status      string          `json:"status"`
		Card        json.RawMessage `json:"card"`
		Version     int             `json:"version"`
	}
	result := make([]cardResponse, len(cards))
	for i, c := range cards {
		result[i] = cardResponse{
			AgentID:     c.SandboxID,
			DisplayName: c.DisplayName,
			Description: c.Description,
			AgentType:   c.AgentType,
			Status:      c.AgentStatus,
			Card:        c.CardJSON,
			Version:     c.Version,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleAgentCreateTask is the proxy_token-authed version of handleCreateTask.
// POST /api/agent/tasks
func (s *Server) handleAgentCreateTask(w http.ResponseWriter, r *http.Request) {
	sbx := s.extractProxyTokenSandbox(w, r)
	if sbx == nil {
		return
	}
	s.handleCreateTaskForWorkspace(w, r, sbx.WorkspaceID)
}

// handleAgentGetTask is the proxy_token-authed version of handleGetTask.
// GET /api/agent/tasks/{id}
func (s *Server) handleAgentGetTask(w http.ResponseWriter, r *http.Request) {
	sbx := s.extractProxyTokenSandbox(w, r)
	if sbx == nil {
		return
	}
	s.handleGetTask(w, r)
}
