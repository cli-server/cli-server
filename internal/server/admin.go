package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
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

func (s *Server) handleAdminGetQuotaDefaults(w http.ResponseWriter, r *http.Request) {
	rd := s.getResourceDefaults()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"maxWorkspacesPerUser":     rd.MaxWorkspacesPerUser,
		"maxSandboxesPerWorkspace": rd.MaxSandboxesPerWorkspace,
		"workspaceDriveSize":       rd.WorkspaceDriveSize,
		"sandboxCpu":               rd.SandboxCPU,
		"sandboxMemory":            rd.SandboxMemory,
		"idleTimeout":              rd.IdleTimeout,
		"wsMaxTotalCpu":            rd.WsMaxTotalCPU,
		"wsMaxTotalMemory":         rd.WsMaxTotalMemory,
		"wsMaxIdleTimeout":         rd.WsMaxIdleTimeout,
	})
}

func (s *Server) handleAdminSetQuotaDefaults(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxWorkspacesPerUser     *int    `json:"maxWorkspacesPerUser"`
		MaxSandboxesPerWorkspace *int    `json:"maxSandboxesPerWorkspace"`
		WorkspaceDriveSize       *string `json:"workspaceDriveSize"`
		SandboxCPU               *string `json:"sandboxCpu"`
		SandboxMemory            *string `json:"sandboxMemory"`
		IdleTimeout              *string `json:"idleTimeout"`
		WsMaxTotalCPU            *string `json:"wsMaxTotalCpu"`
		WsMaxTotalMemory         *string `json:"wsMaxTotalMemory"`
		WsMaxIdleTimeout         *string `json:"wsMaxIdleTimeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.MaxWorkspacesPerUser != nil {
		if *req.MaxWorkspacesPerUser < 0 {
			http.Error(w, "maxWorkspacesPerUser must be >= 0", http.StatusBadRequest)
			return
		}
		if err := s.DB.SetSystemSetting(settingKeyMaxWorkspaces, strconv.Itoa(*req.MaxWorkspacesPerUser)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.MaxSandboxesPerWorkspace != nil {
		if *req.MaxSandboxesPerWorkspace < 0 {
			http.Error(w, "maxSandboxesPerWorkspace must be >= 0", http.StatusBadRequest)
			return
		}
		if err := s.DB.SetSystemSetting(settingKeyMaxSandboxes, strconv.Itoa(*req.MaxSandboxesPerWorkspace)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.WorkspaceDriveSize != nil {
		if err := s.DB.SetSystemSetting(settingKeyWorkspaceDriveSize, *req.WorkspaceDriveSize); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.SandboxCPU != nil {
		if err := s.DB.SetSystemSetting(settingKeySandboxCPU, *req.SandboxCPU); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.SandboxMemory != nil {
		if err := s.DB.SetSystemSetting(settingKeySandboxMemory, *req.SandboxMemory); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.IdleTimeout != nil {
		if err := s.DB.SetSystemSetting(settingKeyIdleTimeout, *req.IdleTimeout); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.WsMaxTotalCPU != nil {
		if err := s.DB.SetSystemSetting(settingKeyWsMaxTotalCPU, *req.WsMaxTotalCPU); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.WsMaxTotalMemory != nil {
		if err := s.DB.SetSystemSetting(settingKeyWsMaxTotalMemory, *req.WsMaxTotalMemory); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.WsMaxIdleTimeout != nil {
		if err := s.DB.SetSystemSetting(settingKeyWsMaxIdleTimeout, *req.WsMaxIdleTimeout); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}

	rd := s.getResourceDefaults()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"maxWorkspacesPerUser":     rd.MaxWorkspacesPerUser,
		"maxSandboxesPerWorkspace": rd.MaxSandboxesPerWorkspace,
		"workspaceDriveSize":       rd.WorkspaceDriveSize,
		"sandboxCpu":               rd.SandboxCPU,
		"sandboxMemory":            rd.SandboxMemory,
		"idleTimeout":              rd.IdleTimeout,
		"wsMaxTotalCpu":            rd.WsMaxTotalCPU,
		"wsMaxTotalMemory":         rd.WsMaxTotalMemory,
		"wsMaxIdleTimeout":         rd.WsMaxIdleTimeout,
	})
}

func (s *Server) handleAdminGetUserQuota(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "id")

	rd := s.getResourceDefaults()
	defaults := map[string]interface{}{
		"maxWorkspacesPerUser":     rd.MaxWorkspacesPerUser,
		"maxSandboxesPerWorkspace": rd.MaxSandboxesPerWorkspace,
		"workspaceDriveSize":       rd.WorkspaceDriveSize,
		"sandboxCpu":               rd.SandboxCPU,
		"sandboxMemory":            rd.SandboxMemory,
		"idleTimeout":              rd.IdleTimeout,
		"wsMaxTotalCpu":            rd.WsMaxTotalCPU,
		"wsMaxTotalMemory":         rd.WsMaxTotalMemory,
		"wsMaxIdleTimeout":         rd.WsMaxIdleTimeout,
	}

	uq, err := s.DB.GetUserQuota(targetID)
	if err != nil {
		log.Printf("admin: failed to get user quota: %v", err)
		http.Error(w, "failed to get user quota", http.StatusInternalServerError)
		return
	}

	var overrides interface{}
	if uq != nil {
		overrides = map[string]interface{}{
			"maxWorkspaces":            uq.MaxWorkspaces,
			"maxSandboxesPerWorkspace": uq.MaxSandboxesPerWorkspace,
			"workspaceDriveSize":       uq.WorkspaceDriveSize,
			"sandboxCpu":               uq.SandboxCPU,
			"sandboxMemory":            uq.SandboxMemory,
			"idleTimeout":              uq.IdleTimeout,
			"wsMaxTotalCpu":            uq.WsMaxTotalCPU,
			"wsMaxTotalMemory":         uq.WsMaxTotalMemory,
			"wsMaxIdleTimeout":         uq.WsMaxIdleTimeout,
			"updatedAt":               uq.UpdatedAt.Format(time.RFC3339),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"defaults":  defaults,
		"overrides": overrides,
	})
}

func (s *Server) handleAdminSetUserQuota(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "id")

	var req struct {
		MaxWorkspaces            *int    `json:"maxWorkspaces"`
		MaxSandboxesPerWorkspace *int    `json:"maxSandboxesPerWorkspace"`
		WorkspaceDriveSize       *string `json:"workspaceDriveSize"`
		SandboxCPU               *string `json:"sandboxCpu"`
		SandboxMemory            *string `json:"sandboxMemory"`
		IdleTimeout              *string `json:"idleTimeout"`
		WsMaxTotalCPU            *string `json:"wsMaxTotalCpu"`
		WsMaxTotalMemory         *string `json:"wsMaxTotalMemory"`
		WsMaxIdleTimeout         *string `json:"wsMaxIdleTimeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.MaxWorkspaces != nil && *req.MaxWorkspaces < 0 {
		http.Error(w, "maxWorkspaces must be >= 0", http.StatusBadRequest)
		return
	}
	if req.MaxSandboxesPerWorkspace != nil && *req.MaxSandboxesPerWorkspace < 0 {
		http.Error(w, "maxSandboxesPerWorkspace must be >= 0", http.StatusBadRequest)
		return
	}

	// Fetch existing to merge partial updates.
	existing, err := s.DB.GetUserQuota(targetID)
	if err != nil {
		log.Printf("admin: failed to get user quota: %v", err)
		http.Error(w, "failed to get user quota", http.StatusInternalServerError)
		return
	}

	mergedWs := req.MaxWorkspaces
	mergedSbx := req.MaxSandboxesPerWorkspace
	mergedDriveSize := req.WorkspaceDriveSize
	mergedSandboxCPU := req.SandboxCPU
	mergedSandboxMemory := req.SandboxMemory
	mergedIdleTimeout := req.IdleTimeout
	mergedWsMaxCPU := req.WsMaxTotalCPU
	mergedWsMaxMemory := req.WsMaxTotalMemory
	mergedWsMaxIdle := req.WsMaxIdleTimeout

	if existing != nil {
		if mergedWs == nil {
			mergedWs = existing.MaxWorkspaces
		}
		if mergedSbx == nil {
			mergedSbx = existing.MaxSandboxesPerWorkspace
		}
		if mergedDriveSize == nil {
			mergedDriveSize = existing.WorkspaceDriveSize
		}
		if mergedSandboxCPU == nil {
			mergedSandboxCPU = existing.SandboxCPU
		}
		if mergedSandboxMemory == nil {
			mergedSandboxMemory = existing.SandboxMemory
		}
		if mergedIdleTimeout == nil {
			mergedIdleTimeout = existing.IdleTimeout
		}
		if mergedWsMaxCPU == nil {
			mergedWsMaxCPU = existing.WsMaxTotalCPU
		}
		if mergedWsMaxMemory == nil {
			mergedWsMaxMemory = existing.WsMaxTotalMemory
		}
		if mergedWsMaxIdle == nil {
			mergedWsMaxIdle = existing.WsMaxIdleTimeout
		}
	}

	if err := s.DB.SetUserQuota(targetID, mergedWs, mergedSbx,
		mergedDriveSize, mergedSandboxCPU, mergedSandboxMemory, mergedIdleTimeout,
		mergedWsMaxCPU, mergedWsMaxMemory, mergedWsMaxIdle); err != nil {
		log.Printf("admin: failed to set user quota: %v", err)
		http.Error(w, fmt.Sprintf("failed to set user quota: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminDeleteUserQuota(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "id")

	if err := s.DB.DeleteUserQuota(targetID); err != nil {
		log.Printf("admin: failed to delete user quota: %v", err)
		http.Error(w, "failed to delete user quota", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
