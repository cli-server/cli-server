package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/agentserver/agentserver/internal/auth"
)

// requireAdmin is a middleware that checks if the authenticated user has the admin role.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := auth.UserIDFromContext(r.Context())
		user, err := s.Auth.GetUserByID(userID)
		if err != nil || user == nil {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		if user.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.DB.ListAllUsers()
	if err != nil {
		log.Printf("admin: failed to list users: %v", err)
		http.Error(w, "failed to list users", http.StatusInternalServerError)
		return
	}

	type adminUserResponse struct {
		ID        string  `json:"id"`
		Username  string  `json:"username"`
		Email     *string `json:"email"`
		Role      string  `json:"role"`
		CreatedAt string  `json:"createdAt"`
	}

	resp := make([]adminUserResponse, len(users))
	for i, u := range users {
		resp[i] = adminUserResponse{
			ID:        u.ID,
			Username:  u.Username,
			Email:     u.Email,
			Role:      u.Role,
			CreatedAt: u.CreatedAt.Format(time.RFC3339),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAdminListWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.DB.ListAllWorkspaces()
	if err != nil {
		log.Printf("admin: failed to list workspaces: %v", err)
		http.Error(w, "failed to list workspaces", http.StatusInternalServerError)
		return
	}

	resp := make([]workspaceResponse, len(workspaces))
	for i, ws := range workspaces {
		resp[i] = s.toWorkspaceResponse(ws)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAdminListSandboxes(w http.ResponseWriter, r *http.Request) {
	sandboxes, err := s.DB.ListAllSandboxes()
	if err != nil {
		log.Printf("admin: failed to list sandboxes: %v", err)
		http.Error(w, "failed to list sandboxes", http.StatusInternalServerError)
		return
	}

	type adminSandboxResponse struct {
		ID             string  `json:"id"`
		Name           string  `json:"name"`
		WorkspaceID    string  `json:"workspaceId"`
		Type           string  `json:"type"`
		Status         string  `json:"status"`
		CreatedAt      string  `json:"createdAt"`
		LastActivityAt *string `json:"lastActivityAt"`
		IsLocal        bool    `json:"isLocal"`
	}

	resp := make([]adminSandboxResponse, len(sandboxes))
	for i, sbx := range sandboxes {
		r := adminSandboxResponse{
			ID:          sbx.ID,
			Name:        sbx.Name,
			WorkspaceID: sbx.WorkspaceID,
			Type:        sbx.Type,
			Status:      sbx.Status,
			CreatedAt:   sbx.CreatedAt.Format(time.RFC3339),
			IsLocal:     sbx.IsLocal,
		}
		if sbx.LastActivityAt.Valid {
			s := sbx.LastActivityAt.Time.Format(time.RFC3339)
			r.LastActivityAt = &s
		}
		resp[i] = r
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAdminUpdateUserRole(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "id")

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Role == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Role != "user" && req.Role != "admin" {
		http.Error(w, "invalid role: must be 'user' or 'admin'", http.StatusBadRequest)
		return
	}

	if err := s.DB.UpdateUserRole(targetID, req.Role); err != nil {
		log.Printf("admin: failed to update user role: %v", err)
		http.Error(w, "failed to update user role", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
