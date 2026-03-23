package server

import (
	"encoding/json"
	"log"
	"net/http"
)

// handleValidateProxyToken is an internal API for the LLM proxy to validate
// sandbox proxy tokens. It returns sandbox metadata without requiring cookie auth.
func (s *Server) handleValidateProxyToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyToken string `json:"proxy_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProxyToken == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sbx, err := s.DB.GetSandboxByProxyToken(req.ProxyToken)
	if err != nil {
		log.Printf("validate-proxy-token: db error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sbx == nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	resp := map[string]interface{}{
		"sandbox_id":   sbx.ID,
		"workspace_id": sbx.WorkspaceID,
		"status":       sbx.Status,
	}

	// Include modelserver upstream URL if workspace has a connection
	if s.ModelserverProxyURL != "" {
		hasMSConn, _ := s.DB.HasModelserverConnection(sbx.WorkspaceID)
		if hasMSConn {
			resp["modelserver_upstream_url"] = s.ModelserverProxyURL
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
