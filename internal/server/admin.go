package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
		Email     string  `json:"email"`
		Name      *string `json:"name"`
		Role      string  `json:"role"`
		CreatedAt string  `json:"created_at"`
	}

	resp := make([]adminUserResponse, len(users))
	for i, u := range users {
		resp[i] = adminUserResponse{
			ID:        u.ID,
			Email:     u.Email,
			Name:      u.Name,
			Role:      u.Role,
			CreatedAt: u.CreatedAt.Format(time.RFC3339),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAdminListWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.DB.ListAllWorkspacesAdmin()
	if err != nil {
		log.Printf("admin: failed to list workspaces: %v", err)
		http.Error(w, "failed to list workspaces", http.StatusInternalServerError)
		return
	}

	rd := s.getResourceDefaults()

	type ownerInfo struct {
		ID      string  `json:"id"`
		Email   string  `json:"email"`
		Name    *string `json:"name"`
		Picture *string `json:"picture"`
	}
	type adminWorkspaceResponse struct {
		ID            string    `json:"id"`
		Name          string    `json:"name"`
		CreatedAt     string    `json:"created_at"`
		UpdatedAt     string    `json:"updated_at"`
		Owner         *ownerInfo `json:"owner"`
		SandboxCount  int       `json:"sandbox_count"`
		MaxSandboxes  int       `json:"max_sandboxes"`
	}

	resp := make([]adminWorkspaceResponse, len(workspaces))
	for i, ws := range workspaces {
		r := adminWorkspaceResponse{
			ID:           ws.ID,
			Name:         ws.Name,
			CreatedAt:    ws.CreatedAt.Format(time.RFC3339),
			UpdatedAt:    ws.UpdatedAt.Format(time.RFC3339),
			SandboxCount: ws.SandboxCount,
			MaxSandboxes: rd.MaxSandboxesPerWorkspace,
		}
		if ws.OwnerID != nil {
			ownerEmail := ""
			if ws.OwnerEmail != nil {
				ownerEmail = *ws.OwnerEmail
			}
			r.Owner = &ownerInfo{
				ID:      *ws.OwnerID,
				Email:   ownerEmail,
				Name:    ws.OwnerName,
				Picture: ws.OwnerPicture,
			}
		}
		// Check for workspace-level quota override.
		if wq, err := s.DB.GetWorkspaceQuota(ws.ID); err == nil && wq != nil && wq.MaxSandboxes != nil {
			r.MaxSandboxes = *wq.MaxSandboxes
		}
		resp[i] = r
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
		WorkspaceID    string  `json:"workspace_id"`
		Type           string  `json:"type"`
		Status         string  `json:"status"`
		CreatedAt      string  `json:"created_at"`
		LastActivityAt *string `json:"last_activity_at"`
		IsLocal        bool    `json:"is_local"`
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
		"max_workspaces_per_user":      rd.MaxWorkspacesPerUser,
		"max_sandboxes_per_workspace":  rd.MaxSandboxesPerWorkspace,
		"max_workspace_drive_size":     rd.MaxWorkspaceDriveSize,
		"max_sandbox_cpu":              rd.MaxSandboxCPU,
		"max_sandbox_memory":           rd.MaxSandboxMemory,
		"max_idle_timeout":             rd.MaxIdleTimeout,
		"ws_max_total_cpu":             rd.WsMaxTotalCPU,
		"ws_max_total_memory":          rd.WsMaxTotalMemory,
		"ws_max_idle_timeout":          rd.WsMaxIdleTimeout,
	})
}

func (s *Server) handleAdminSetQuotaDefaults(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxWorkspacesPerUser     *int   `json:"max_workspaces_per_user"`
		MaxSandboxesPerWorkspace *int   `json:"max_sandboxes_per_workspace"`
		MaxWorkspaceDriveSize    *int64 `json:"max_workspace_drive_size"`
		MaxSandboxCPU            *int   `json:"max_sandbox_cpu"`
		MaxSandboxMemory         *int64 `json:"max_sandbox_memory"`
		MaxIdleTimeout           *int   `json:"max_idle_timeout"`
		WsMaxTotalCPU            *int   `json:"ws_max_total_cpu"`
		WsMaxTotalMemory         *int64 `json:"ws_max_total_memory"`
		WsMaxIdleTimeout         *int   `json:"ws_max_idle_timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.MaxWorkspacesPerUser != nil {
		if *req.MaxWorkspacesPerUser < 0 {
			http.Error(w, "max_workspaces_per_user must be >= 0", http.StatusBadRequest)
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
			http.Error(w, "max_sandboxes_per_workspace must be >= 0", http.StatusBadRequest)
			return
		}
		if err := s.DB.SetSystemSetting(settingKeyMaxSandboxes, strconv.Itoa(*req.MaxSandboxesPerWorkspace)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.MaxWorkspaceDriveSize != nil {
		if err := s.DB.SetSystemSetting(settingKeyMaxWorkspaceDriveSize, strconv.FormatInt(*req.MaxWorkspaceDriveSize, 10)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.MaxSandboxCPU != nil {
		if err := s.DB.SetSystemSetting(settingKeyMaxSandboxCPU, strconv.Itoa(*req.MaxSandboxCPU)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.MaxSandboxMemory != nil {
		if err := s.DB.SetSystemSetting(settingKeyMaxSandboxMemory, strconv.FormatInt(*req.MaxSandboxMemory, 10)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.MaxIdleTimeout != nil {
		if err := s.DB.SetSystemSetting(settingKeyMaxIdleTimeout, strconv.Itoa(*req.MaxIdleTimeout)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.WsMaxTotalCPU != nil {
		if err := s.DB.SetSystemSetting(settingKeyWsMaxTotalCPU, strconv.Itoa(*req.WsMaxTotalCPU)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.WsMaxTotalMemory != nil {
		if err := s.DB.SetSystemSetting(settingKeyWsMaxTotalMemory, strconv.FormatInt(*req.WsMaxTotalMemory, 10)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}
	if req.WsMaxIdleTimeout != nil {
		if err := s.DB.SetSystemSetting(settingKeyWsMaxIdleTimeout, strconv.Itoa(*req.WsMaxIdleTimeout)); err != nil {
			log.Printf("admin: failed to set quota default: %v", err)
			http.Error(w, "failed to save setting", http.StatusInternalServerError)
			return
		}
	}

	rd := s.getResourceDefaults()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"max_workspaces_per_user":      rd.MaxWorkspacesPerUser,
		"max_sandboxes_per_workspace":  rd.MaxSandboxesPerWorkspace,
		"max_workspace_drive_size":     rd.MaxWorkspaceDriveSize,
		"max_sandbox_cpu":              rd.MaxSandboxCPU,
		"max_sandbox_memory":           rd.MaxSandboxMemory,
		"max_idle_timeout":             rd.MaxIdleTimeout,
		"ws_max_total_cpu":             rd.WsMaxTotalCPU,
		"ws_max_total_memory":          rd.WsMaxTotalMemory,
		"ws_max_idle_timeout":          rd.WsMaxIdleTimeout,
	})
}

func (s *Server) handleAdminGetUserQuota(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "id")

	rd := s.getResourceDefaults()
	defaults := map[string]interface{}{
		"max_workspaces_per_user": rd.MaxWorkspacesPerUser,
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
			"max_workspaces": uq.MaxWorkspaces,
			"updated_at":     uq.UpdatedAt.Format(time.RFC3339),
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
		MaxWorkspaces *int `json:"max_workspaces"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.MaxWorkspaces != nil && *req.MaxWorkspaces < 0 {
		http.Error(w, "max_workspaces must be >= 0", http.StatusBadRequest)
		return
	}

	if err := s.DB.SetUserQuota(targetID, req.MaxWorkspaces); err != nil {
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

func (s *Server) handleAdminGetWorkspaceQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")

	rd := s.getResourceDefaults()
	defaults := map[string]interface{}{
		"max_sandboxes":      rd.MaxSandboxesPerWorkspace,
		"max_sandbox_cpu":    rd.MaxSandboxCPU,
		"max_sandbox_memory": rd.MaxSandboxMemory,
		"max_idle_timeout":   rd.MaxIdleTimeout,
		"max_total_cpu":      rd.WsMaxTotalCPU,
		"max_total_memory":   rd.WsMaxTotalMemory,
		"max_drive_size":     rd.MaxWorkspaceDriveSize,
	}

	wq, err := s.DB.GetWorkspaceQuota(workspaceID)
	if err != nil {
		log.Printf("admin: failed to get workspace quota: %v", err)
		http.Error(w, "failed to get workspace quota", http.StatusInternalServerError)
		return
	}

	var overrides interface{}
	if wq != nil {
		overrides = map[string]interface{}{
			"max_sandboxes":      wq.MaxSandboxes,
			"max_sandbox_cpu":    wq.MaxSandboxCPU,
			"max_sandbox_memory": wq.MaxSandboxMemory,
			"max_idle_timeout":   wq.MaxIdleTimeout,
			"max_total_cpu":      wq.MaxTotalCPU,
			"max_total_memory":   wq.MaxTotalMemory,
			"max_drive_size":     wq.MaxDriveSize,
			"updated_at":         wq.UpdatedAt.Format(time.RFC3339),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"defaults":  defaults,
		"overrides": overrides,
	})
}

func (s *Server) handleAdminSetWorkspaceQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")

	var req struct {
		MaxSandboxes     *int   `json:"max_sandboxes"`
		MaxSandboxCPU    *int   `json:"max_sandbox_cpu"`
		MaxSandboxMemory *int64 `json:"max_sandbox_memory"`
		MaxIdleTimeout   *int   `json:"max_idle_timeout"`
		MaxTotalCPU      *int   `json:"max_total_cpu"`
		MaxTotalMemory   *int64 `json:"max_total_memory"`
		MaxDriveSize     *int64 `json:"max_drive_size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.MaxSandboxes != nil && *req.MaxSandboxes < 0 {
		http.Error(w, "max_sandboxes must be >= 0", http.StatusBadRequest)
		return
	}

	// Fetch existing to merge partial updates.
	existing, err := s.DB.GetWorkspaceQuota(workspaceID)
	if err != nil {
		log.Printf("admin: failed to get workspace quota: %v", err)
		http.Error(w, "failed to get workspace quota", http.StatusInternalServerError)
		return
	}

	mergedSbx := req.MaxSandboxes
	mergedCPU := req.MaxSandboxCPU
	mergedMemory := req.MaxSandboxMemory
	mergedIdle := req.MaxIdleTimeout
	mergedMaxCPU := req.MaxTotalCPU
	mergedMaxMemory := req.MaxTotalMemory
	mergedDrive := req.MaxDriveSize

	if existing != nil {
		if mergedSbx == nil {
			mergedSbx = existing.MaxSandboxes
		}
		if mergedCPU == nil {
			mergedCPU = existing.MaxSandboxCPU
		}
		if mergedMemory == nil {
			mergedMemory = existing.MaxSandboxMemory
		}
		if mergedIdle == nil {
			mergedIdle = existing.MaxIdleTimeout
		}
		if mergedMaxCPU == nil {
			mergedMaxCPU = existing.MaxTotalCPU
		}
		if mergedMaxMemory == nil {
			mergedMaxMemory = existing.MaxTotalMemory
		}
		if mergedDrive == nil {
			mergedDrive = existing.MaxDriveSize
		}
	}

	if err := s.DB.SetWorkspaceQuota(workspaceID, mergedSbx,
		mergedCPU, mergedMemory, mergedIdle,
		mergedMaxCPU, mergedMaxMemory, mergedDrive); err != nil {
		log.Printf("admin: failed to set workspace quota: %v", err)
		http.Error(w, fmt.Sprintf("failed to set workspace quota: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminDeleteWorkspaceQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")

	if err := s.DB.DeleteWorkspaceQuota(workspaceID); err != nil {
		log.Printf("admin: failed to delete workspace quota: %v", err)
		http.Error(w, "failed to delete workspace quota", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// proxyLLMProxyRequest forwards an HTTP request to the llmproxy internal API.
func (s *Server) proxyLLMProxyRequest(w http.ResponseWriter, method, path string, body []byte) {
	if s.LLMProxyURL == "" {
		http.Error(w, "llmproxy not configured", http.StatusServiceUnavailable)
		return
	}
	url := s.LLMProxyURL + path
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("llmproxy proxy error: %v", err)
		http.Error(w, "llmproxy unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *Server) handleAdminGetWorkspaceLLMQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	s.proxyLLMProxyRequest(w, http.MethodGet, "/internal/quotas/"+workspaceID, nil)
}

func (s *Server) handleAdminSetWorkspaceLLMQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.proxyLLMProxyRequest(w, http.MethodPut, "/internal/quotas/"+workspaceID, body)
}

func (s *Server) handleAdminDeleteWorkspaceLLMQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	s.proxyLLMProxyRequest(w, http.MethodDelete, "/internal/quotas/"+workspaceID, nil)
}
