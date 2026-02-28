package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/imryao/cli-server/internal/auth"
	"github.com/imryao/cli-server/internal/db"
	"github.com/imryao/cli-server/internal/process"
	"github.com/imryao/cli-server/internal/sbxstore"
	"github.com/imryao/cli-server/internal/storage"
)

type Server struct {
	Auth           *auth.Auth
	OIDC           *auth.OIDCManager
	DB             *db.DB
	Sandboxes      *sbxstore.Store
	ProcessManager process.Manager
	DriveManager   storage.DriveManager
	StaticFS       fs.FS
	BaseDomain     string // e.g. "cli.cs.ac.cn" — used for subdomain routing
	BaseScheme     string // e.g. "https" — scheme for generated URLs
	// activityThrottle prevents excessive DB writes for activity tracking.
	activityMu   sync.Mutex
	activityLast map[string]time.Time
}

func New(a *auth.Auth, oidcMgr *auth.OIDCManager, database *db.DB, sandboxStore *sbxstore.Store, processManager process.Manager, driveManager storage.DriveManager, staticFS fs.FS) *Server {
	baseDomain := os.Getenv("BASE_DOMAIN")
	baseScheme := os.Getenv("BASE_SCHEME")
	if baseScheme == "" {
		baseScheme = "https"
	}

	s := &Server{
		Auth:           a,
		OIDC:           oidcMgr,
		DB:             database,
		Sandboxes:      sandboxStore,
		ProcessManager: processManager,
		DriveManager:   driveManager,
		StaticFS:       staticFS,
		BaseDomain:     baseDomain,
		BaseScheme:     baseScheme,
		activityLast:   make(map[string]time.Time),
	}
	return s
}

// throttledActivity updates activity at most once per 30 seconds per sandbox.
func (s *Server) throttledActivity(sandboxID string) {
	s.activityMu.Lock()
	last, ok := s.activityLast[sandboxID]
	now := time.Now()
	if ok && now.Sub(last) < 30*time.Second {
		s.activityMu.Unlock()
		return
	}
	s.activityLast[sandboxID] = now
	s.activityMu.Unlock()
	s.Sandboxes.UpdateActivity(sandboxID)
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Subdomain middleware: if the Host matches oc-{sandboxID}.{baseDomain},
	// proxy the entire request to the sandbox pod and skip all other routes.
	if s.BaseDomain != "" {
		r.Use(func(next http.Handler) http.Handler {
			suffix := "." + s.BaseDomain
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host := r.Host
				// Strip port if present.
				if idx := strings.LastIndex(host, ":"); idx != -1 {
					host = host[:idx]
				}
				if strings.HasSuffix(host, suffix) {
					sub := strings.TrimSuffix(host, suffix)
					if strings.HasPrefix(sub, "oc-") {
						sandboxID := sub[3:] // strip "oc-" prefix
						s.handleSubdomainProxy(w, r, sandboxID)
						return
					}
					if strings.HasPrefix(sub, "claw-") {
						sandboxID := sub[5:] // strip "claw-" prefix
						s.handleOpenclawSubdomainProxy(w, r, sandboxID)
						return
					}
				}
				next.ServeHTTP(w, r)
			})
		})
	}

	// Health endpoint (no auth required, for K8s probes)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Anthropic API proxy for sandboxes (auth via proxy token in x-api-key header).
	r.HandleFunc("/proxy/anthropic/*", s.handleAnthropicProxy)

	// Auth endpoints (no auth required)
	r.Post("/api/auth/login", s.handleLogin)
	r.Post("/api/auth/register", s.handleRegister)
	r.Get("/api/auth/check", s.handleAuthCheck)
	r.Post("/api/auth/logout", s.handleLogout)

	// OIDC endpoints (no auth required)
	if s.OIDC != nil {
		r.Get("/api/auth/oidc/providers", s.OIDC.HandleProviders)
		r.Get("/api/auth/oidc/{provider}/login", s.handleOIDCLogin)
		r.Get("/api/auth/oidc/{provider}/callback", s.handleOIDCCallback)
	} else {
		r.Get("/api/auth/oidc/providers", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"providers": []string{}})
		})
	}

	// Protected API routes
	r.Group(func(r chi.Router) {
		r.Use(s.Auth.Middleware)

		r.Get("/api/auth/me", s.handleMe)

		// Workspace routes
		r.Get("/api/workspaces", s.handleListWorkspaces)
		r.Post("/api/workspaces", s.handleCreateWorkspace)
		r.Get("/api/workspaces/{id}", s.handleGetWorkspace)
		r.Delete("/api/workspaces/{id}", s.handleDeleteWorkspace)

		// Workspace member routes
		r.Get("/api/workspaces/{id}/members", s.handleListMembers)
		r.Post("/api/workspaces/{id}/members", s.handleAddMember)
		r.Put("/api/workspaces/{id}/members/{userId}", s.handleUpdateMemberRole)
		r.Delete("/api/workspaces/{id}/members/{userId}", s.handleRemoveMember)

		// Sandbox routes
		r.Get("/api/workspaces/{wid}/sandboxes", s.handleListSandboxes)
		r.Post("/api/workspaces/{wid}/sandboxes", s.handleCreateSandbox)
		r.Get("/api/sandboxes/{id}", s.handleGetSandbox)
		r.Delete("/api/sandboxes/{id}", s.handleDeleteSandbox)
		r.Post("/api/sandboxes/{id}/pause", s.handlePauseSandbox)
		r.Post("/api/sandboxes/{id}/resume", s.handleResumeSandbox)
	})

	// Static files
	if s.StaticFS != nil {
		fileServer := http.FileServer(http.FS(s.StaticFS))
		r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			upath := r.URL.Path
			if upath == "/" {
				upath = "/index.html"
			}
			if _, err := fs.Stat(s.StaticFS, upath[1:]); err != nil {
				// SPA fallback: serve index.html for client-side routes.
				r.URL.Path = "/"
			}
			fileServer.ServeHTTP(w, r)
		})
	}

	return r
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token, _, ok := s.Auth.Login(req.Username, req.Password)
	if !ok {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	auth.SetTokenCookie(w, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Username == "" || req.Password == "" {
		http.Error(w, "username and password required", http.StatusBadRequest)
		return
	}

	// Check if user already exists.
	existing, err := s.Auth.GetUserByUsername(req.Username)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		http.Error(w, "username already taken", http.StatusConflict)
		return
	}

	id := uuid.New().String()
	if err := s.Auth.Register(id, req.Username, req.Password); err != nil {
		log.Printf("register error: %v", err)
		http.Error(w, "failed to create user", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "username": req.Username})
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Auth.ValidateRequest(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "cli-server-token",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	user, err := s.Auth.GetUserByID(userID)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       user.ID,
		"username": user.Username,
		"email":    user.Email,
	})
}

// --- Response types ---

type workspaceResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	DiskPVCName *string `json:"diskPvcName,omitempty"`
	CreatedAt  string  `json:"createdAt"`
	UpdatedAt  string  `json:"updatedAt"`
}

type workspaceMemberResponse struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type sandboxResponse struct {
	ID             string  `json:"id"`
	WorkspaceID    string  `json:"workspaceId"`
	Name           string  `json:"name"`
	Type           string  `json:"type"`
	Status         string  `json:"status"`
	OpencodeURL    string  `json:"opencodeUrl,omitempty"`
	OpenclawURL    string  `json:"openclawUrl,omitempty"`
	CreatedAt      string  `json:"createdAt"`
	LastActivityAt *string `json:"lastActivityAt"`
	PausedAt       *string `json:"pausedAt"`
}

func (s *Server) toWorkspaceResponse(ws *db.Workspace) workspaceResponse {
	resp := workspaceResponse{
		ID:        ws.ID,
		Name:      ws.Name,
		CreatedAt: ws.CreatedAt.Format(time.RFC3339),
		UpdatedAt: ws.UpdatedAt.Format(time.RFC3339),
	}
	if ws.DiskPVCName.Valid {
		resp.DiskPVCName = &ws.DiskPVCName.String
	}
	return resp
}

func (s *Server) toSandboxResponse(sbx *sbxstore.Sandbox, authToken string) sandboxResponse {
	resp := sandboxResponse{
		ID:          sbx.ID,
		WorkspaceID: sbx.WorkspaceID,
		Name:        sbx.Name,
		Type:        sbx.Type,
		Status:      sbx.Status,
		CreatedAt:   sbx.CreatedAt.Format(time.RFC3339),
	}
	if s.BaseDomain != "" {
		switch sbx.Type {
		case "openclaw":
			resp.OpenclawURL = s.BaseScheme + "://claw-" + sbx.ID + "." + s.BaseDomain + "/auth?token=" + authToken
		default: // "opencode"
			resp.OpencodeURL = s.BaseScheme + "://oc-" + sbx.ID + "." + s.BaseDomain + "/auth?token=" + authToken
		}
	}
	if sbx.LastActivityAt != nil {
		s := sbx.LastActivityAt.Format(time.RFC3339)
		resp.LastActivityAt = &s
	}
	if sbx.PausedAt != nil {
		s := sbx.PausedAt.Format(time.RFC3339)
		resp.PausedAt = &s
	}
	return resp
}

// authTokenFromRequest extracts the raw auth token from the request cookie.
func authTokenFromRequest(r *http.Request) string {
	c, err := r.Cookie("cli-server-token")
	if err != nil {
		return ""
	}
	return c.Value
}

// --- Authorization helpers ---

func (s *Server) requireWorkspaceMember(w http.ResponseWriter, r *http.Request, workspaceID string) (string, bool) {
	userID := auth.UserIDFromContext(r.Context())
	role, err := s.DB.GetWorkspaceMemberRole(workspaceID, userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", false
	}
	if role == "" {
		http.Error(w, "not a workspace member", http.StatusForbidden)
		return "", false
	}
	return role, true
}

func (s *Server) requireWorkspaceRole(w http.ResponseWriter, r *http.Request, workspaceID string, allowedRoles ...string) bool {
	role, ok := s.requireWorkspaceMember(w, r, workspaceID)
	if !ok {
		return false
	}
	for _, allowed := range allowedRoles {
		if role == allowed {
			return true
		}
	}
	http.Error(w, "insufficient permissions", http.StatusForbidden)
	return false
}

// --- Workspace handlers ---

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	workspaces, err := s.DB.ListWorkspacesByUser(userID)
	if err != nil {
		log.Printf("failed to list workspaces: %v", err)
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

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Name = "New Workspace"
	}
	if req.Name == "" {
		req.Name = "New Workspace"
	}

	id := uuid.New().String()
	if err := s.DB.CreateWorkspace(id, req.Name); err != nil {
		log.Printf("failed to create workspace: %v", err)
		http.Error(w, "failed to create workspace", http.StatusInternalServerError)
		return
	}

	// Add creator as owner.
	if err := s.DB.AddWorkspaceMember(id, userID, "owner"); err != nil {
		log.Printf("failed to add workspace owner: %v", err)
		s.DB.DeleteWorkspace(id)
		http.Error(w, "failed to create workspace", http.StatusInternalServerError)
		return
	}

	ws, err := s.DB.GetWorkspace(id)
	if err != nil || ws == nil {
		http.Error(w, "failed to get workspace", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(s.toWorkspaceResponse(ws))
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := s.requireWorkspaceMember(w, r, id); !ok {
		return
	}

	ws, err := s.DB.GetWorkspace(id)
	if err != nil || ws == nil {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.toWorkspaceResponse(ws))
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, id, "owner") {
		return
	}

	// Stop all sandboxes in the workspace.
	sandboxes := s.Sandboxes.ListByWorkspace(id)
	for _, sbx := range sandboxes {
		switch sbx.Status {
		case sbxstore.StatusRunning:
			s.ProcessManager.Stop(sbx.ID)
		case sbxstore.StatusPaused:
			if sbx.SandboxName != "" {
				switch mgr := s.ProcessManager.(type) {
				case interface{ StopBySandboxName(string) error }:
					mgr.StopBySandboxName(sbx.SandboxName)
				case interface{ StopByContainerName(string) error }:
					mgr.StopByContainerName(sbx.SandboxName)
				}
			}
		}
	}

	if err := s.DB.DeleteWorkspace(id); err != nil {
		log.Printf("failed to delete workspace %s: %v", id, err)
		http.Error(w, "failed to delete workspace", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Member handlers ---

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	members, err := s.DB.ListWorkspaceMembers(wsID)
	if err != nil {
		log.Printf("failed to list members: %v", err)
		http.Error(w, "failed to list members", http.StatusInternalServerError)
		return
	}

	resp := make([]workspaceMemberResponse, 0, len(members))
	for _, m := range members {
		user, err := s.Auth.GetUserByID(m.UserID)
		username := m.UserID
		if err == nil && user != nil {
			username = user.Username
		}
		resp = append(resp, workspaceMemberResponse{
			UserID:   m.UserID,
			Username: username,
			Role:     m.Role,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer") {
		return
	}

	var req struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = "developer"
	}

	user, err := s.Auth.GetUserByUsername(req.Username)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	if err := s.DB.AddWorkspaceMember(wsID, user.ID, req.Role); err != nil {
		log.Printf("failed to add member: %v", err)
		http.Error(w, "failed to add member", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(workspaceMemberResponse{
		UserID:   user.ID,
		Username: user.Username,
		Role:     req.Role,
	})
}

func (s *Server) handleUpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner") {
		return
	}

	targetUserID := chi.URLParam(r, "userId")
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Role == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := s.DB.UpdateWorkspaceMemberRole(wsID, targetUserID, req.Role); err != nil {
		log.Printf("failed to update member role: %v", err)
		http.Error(w, "failed to update member role", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner") {
		return
	}

	targetUserID := chi.URLParam(r, "userId")
	if err := s.DB.RemoveWorkspaceMember(wsID, targetUserID); err != nil {
		log.Printf("failed to remove member: %v", err)
		http.Error(w, "failed to remove member", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Sandbox handlers ---

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wid")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	sandboxes := s.Sandboxes.ListByWorkspace(wsID)
	token := authTokenFromRequest(r)
	resp := make([]sandboxResponse, len(sandboxes))
	for i, sbx := range sandboxes {
		resp[i] = s.toSandboxResponse(sbx, token)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wid")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer", "developer") {
		return
	}

	var req struct {
		Name             string `json:"name"`
		Type             string `json:"type"`
		TelegramBotToken string `json:"telegramBotToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Name = "New Sandbox"
	}
	if req.Name == "" {
		req.Name = "New Sandbox"
	}
	sandboxType := req.Type
	if sandboxType == "" {
		sandboxType = "opencode"
	}
	if sandboxType != "opencode" && sandboxType != "openclaw" {
		http.Error(w, "invalid sandbox type: must be opencode or openclaw", http.StatusBadRequest)
		return
	}

	// Ensure workspace drive exists.
	workspaceDiskPVC, err := s.DriveManager.EnsureDrive(r.Context(), wsID)
	if err != nil {
		log.Printf("failed to ensure workspace drive for %s: %v", wsID, err)
		// Non-fatal: sandbox can still work without workspace drive.
	}

	id := uuid.New().String()
	sandboxName := "cli-sandbox-" + shortID(id)

	// Generate auth credentials based on sandbox type.
	var opencodePassword, gatewayToken string
	proxyToken := generatePassword()
	switch sandboxType {
	case "openclaw":
		gatewayToken = generatePassword()
	default: // "opencode"
		opencodePassword = generatePassword()
	}

	sbx, err := s.Sandboxes.Create(id, wsID, req.Name, sandboxType, sandboxName, opencodePassword, proxyToken, req.TelegramBotToken, gatewayToken)
	if err != nil {
		log.Printf("failed to create sandbox: %v", err)
		http.Error(w, "failed to create sandbox", http.StatusInternalServerError)
		return
	}

	// Start container asynchronously.
	go func() {
		var podIP string
		// Use StartContainerWithIP if available (K8s backend) to get the pod IP.
		if sc, ok := s.ProcessManager.(interface {
			StartContainerWithIP(string, process.StartOptions) (string, error)
		}); ok {
			var err error
			podIP, err = sc.StartContainerWithIP(id, process.StartOptions{
				WorkspaceDiskPVC: workspaceDiskPVC,
				OpencodePassword: opencodePassword,
				ProxyToken:       proxyToken,
				SandboxType:      sandboxType,
				TelegramBotToken: req.TelegramBotToken,
				GatewayToken:     gatewayToken,
			})
			if err != nil {
				log.Printf("failed to start container for sandbox %s: %v", id, err)
				s.Sandboxes.Delete(id)
				return
			}
		} else {
			if err := s.ProcessManager.StartContainer(id, process.StartOptions{
				WorkspaceDiskPVC: workspaceDiskPVC,
				OpencodePassword: opencodePassword,
				ProxyToken:       proxyToken,
				SandboxType:      sandboxType,
				TelegramBotToken: req.TelegramBotToken,
				GatewayToken:     gatewayToken,
			}); err != nil {
				log.Printf("failed to start container for sandbox %s: %v", id, err)
				s.Sandboxes.Delete(id)
				return
			}
		}
		if podIP != "" {
			if err := s.DB.UpdateSandboxPodIP(id, podIP); err != nil {
				log.Printf("failed to update pod IP for sandbox %s: %v", id, err)
			}
		}
		s.Sandboxes.UpdateStatus(id, sbxstore.StatusRunning)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(s.toSandboxResponse(sbx, authTokenFromRequest(r)))
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.toSandboxResponse(sbx, authTokenFromRequest(r)))
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	// Handle based on sandbox status.
	switch sbx.Status {
	case sbxstore.StatusRunning:
		s.ProcessManager.Stop(id)
	case sbxstore.StatusPaused:
		if sbx.SandboxName != "" {
			switch mgr := s.ProcessManager.(type) {
			case interface{ StopBySandboxName(string) error }:
				mgr.StopBySandboxName(sbx.SandboxName)
			case interface{ StopByContainerName(string) error }:
				mgr.StopByContainerName(sbx.SandboxName)
			}
		}
	}

	if err := s.Sandboxes.Delete(id); err != nil {
		log.Printf("failed to delete sandbox %s: %v", id, err)
		http.Error(w, "failed to delete sandbox", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePauseSandbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	if !sbxstore.ValidTransition(sbx.Status, sbxstore.StatusPausing) {
		http.Error(w, "sandbox cannot be paused in current state: "+sbx.Status, http.StatusConflict)
		return
	}

	// Transition to pausing.
	if err := s.Sandboxes.UpdateStatus(id, sbxstore.StatusPausing); err != nil {
		http.Error(w, "failed to update status", http.StatusInternalServerError)
		return
	}

	// Pause asynchronously.
	go func() {
		if err := s.ProcessManager.Pause(id); err != nil {
			log.Printf("failed to pause sandbox %s: %v", id, err)
			s.Sandboxes.UpdateStatus(id, sbxstore.StatusRunning)
			return
		}
		// Clear pod IP so the proxy won't connect to a stale address.
		if err := s.DB.UpdateSandboxPodIP(id, ""); err != nil {
			log.Printf("failed to clear pod IP for sandbox %s: %v", id, err)
		}
		s.Sandboxes.UpdateStatus(id, sbxstore.StatusPaused)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "pausing"})
}

func (s *Server) handleResumeSandbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	if !sbxstore.ValidTransition(sbx.Status, sbxstore.StatusResuming) {
		http.Error(w, "sandbox cannot be resumed in current state: "+sbx.Status, http.StatusConflict)
		return
	}

	// Transition to resuming.
	if err := s.Sandboxes.UpdateStatus(id, sbxstore.StatusResuming); err != nil {
		http.Error(w, "failed to update status", http.StatusInternalServerError)
		return
	}

	// Resume asynchronously.
	go func() {
		var err error
		var podIP string
		// Use ResumeContainerWithIP if available (K8s backend).
		if rc, ok := s.ProcessManager.(interface {
			ResumeContainerWithIP(string) (string, error)
		}); ok {
			podIP, err = rc.ResumeContainerWithIP(id)
		} else if rc, ok := s.ProcessManager.(interface{ ResumeContainer(string) error }); ok {
			err = rc.ResumeContainer(id)
		} else {
			err = s.ProcessManager.StartContainer(id, process.StartOptions{})
		}
		if err != nil {
			log.Printf("failed to resume sandbox %s: %v", id, err)
			s.Sandboxes.UpdateStatus(id, sbxstore.StatusPaused)
			return
		}
		if podIP != "" {
			if err := s.DB.UpdateSandboxPodIP(id, podIP); err != nil {
				log.Printf("failed to update pod IP for sandbox %s: %v", id, err)
			}
		}
		s.Sandboxes.UpdateActivity(id)
		s.Sandboxes.UpdateStatus(id, sbxstore.StatusRunning)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resuming"})
}

func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	s.OIDC.HandleLogin(w, r, provider)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	s.OIDC.HandleCallback(w, r, provider)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// generatePassword creates a random 32-character hex password for opencode server auth.
func generatePassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use UUID if crypto/rand fails (should not happen).
		return uuid.New().String()
	}
	return hex.EncodeToString(b)
}
