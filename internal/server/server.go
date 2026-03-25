package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/namespace"
	"github.com/agentserver/agentserver/internal/process"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/shortid"
	"github.com/agentserver/agentserver/internal/storage"
	"github.com/agentserver/agentserver/internal/tunnel"
	"github.com/agentserver/agentserver/internal/weixin"
)

type Server struct {
	Auth             *auth.Auth
	OIDC             *auth.OIDCManager
	DB               *db.DB
	Sandboxes        *sbxstore.Store
	ProcessManager   process.Manager
	DriveManager     storage.DriveManager
	NamespaceManager *namespace.Manager
	TunnelRegistry   *tunnel.Registry
	StaticFS         fs.FS
	BaseDomains              []string // e.g. ["agentserver.dev", "agent.cs.ac.cn"] (first is primary)
	OpencodeSubdomainPrefix  string   // e.g. "code" — subdomain: code-{id}.{baseDomain}
	OpenclawSubdomainPrefix  string // e.g. "claw" — subdomain: claw-{id}.{baseDomain}
	PasswordAuthEnabled      bool   // when false, /api/auth/login and /api/auth/register are not registered
	LLMProxyURL              string // base URL for the llmproxy service (e.g. "http://agentserver-llmproxy:8081")

	// ModelServer OAuth
	ModelserverOAuthClientID      string
	ModelserverOAuthClientSecret  string
	ModelserverOAuthAuthURL       string
	ModelserverOAuthTokenURL      string
	ModelserverOAuthIntrospectURL string
	ModelserverOAuthRedirectURI   string
	ModelserverProxyURL           string
}

func New(a *auth.Auth, oidcMgr *auth.OIDCManager, database *db.DB, sandboxStore *sbxstore.Store, processManager process.Manager, driveManager storage.DriveManager, nsMgr *namespace.Manager, tunnelReg *tunnel.Registry, staticFS fs.FS, passwordAuthEnabled bool) *Server {
	// Parse comma-separated base domains (e.g. "agentserver.dev,agent.cs.ac.cn").
	var baseDomains []string
	if raw := os.Getenv("BASE_DOMAIN"); raw != "" {
		for _, d := range strings.Split(raw, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				baseDomains = append(baseDomains, d)
			}
		}
	}

	opcodePrefix := os.Getenv("OPENCODE_SUBDOMAIN_PREFIX")
	if opcodePrefix == "" {
		opcodePrefix = "code"
	}
	openclawPrefix := os.Getenv("OPENCLAW_SUBDOMAIN_PREFIX")
	if openclawPrefix == "" {
		openclawPrefix = "claw"
	}

	s := &Server{
		Auth:                    a,
		OIDC:                    oidcMgr,
		DB:                      database,
		Sandboxes:               sandboxStore,
		ProcessManager:          processManager,
		DriveManager:            driveManager,
		NamespaceManager:        nsMgr,
		TunnelRegistry:          tunnelReg,
		StaticFS:                staticFS,
		BaseDomains:             baseDomains,
		OpencodeSubdomainPrefix: opcodePrefix,
		OpenclawSubdomainPrefix: openclawPrefix,
		PasswordAuthEnabled:     passwordAuthEnabled,
	}
	if s.OIDC != nil {
		s.OIDC.OnUserCreated = s.createDefaultWorkspace
	}
	return s
}

// createDefaultWorkspace creates a "Default workspace" for a newly registered user.
func (s *Server) createDefaultWorkspace(userID string) {
	id := uuid.New().String()
	if err := s.DB.CreateWorkspace(id, "Default workspace"); err != nil {
		log.Printf("failed to create default workspace for user %s: %v", userID, err)
		return
	}
	if err := s.DB.AddWorkspaceMember(id, userID, "owner"); err != nil {
		log.Printf("failed to add owner to default workspace for user %s: %v", userID, err)
		s.DB.DeleteWorkspace(id)
		return
	}
	if s.NamespaceManager != nil {
		ns, err := s.NamespaceManager.EnsureNamespace(context.Background(), id)
		if err != nil {
			log.Printf("failed to create namespace for default workspace %s: %v", id, err)
			return
		}
		if err := s.DB.SetWorkspaceNamespace(id, ns); err != nil {
			log.Printf("failed to set namespace for default workspace %s: %v", id, err)
		}
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Health endpoint (no auth required, for K8s probes)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Internal API for LLM proxy token validation (no cookie auth).
	r.Post("/internal/validate-proxy-token", s.handleValidateProxyToken)

	// Internal API for ModelServer token retrieval (no cookie auth).
	r.Get("/internal/workspaces/{id}/modelserver-token", s.handleInternalModelserverToken)

	// Agent registration (auth via one-time code, no cookie auth needed).
	r.Post("/api/agent/register", s.handleAgentRegister)

	// Auth endpoints (no auth required)
	if s.PasswordAuthEnabled {
		r.Post("/api/auth/login", s.handleLogin)
		r.Post("/api/auth/register", s.handleRegister)
	}
	r.Get("/api/auth/check", s.handleAuthCheck)
	r.Post("/api/auth/logout", s.handleLogout)

	// OIDC endpoints (no auth required)
	if s.OIDC != nil {
		r.Get("/api/auth/oidc/providers", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"providers":     s.OIDC.ProviderNamesForHost(r.Host),
				"password_auth": s.PasswordAuthEnabled,
			})
		})
		r.Get("/api/auth/oidc/{provider}/login", s.handleOIDCLogin)
		r.Get("/api/auth/oidc/{provider}/callback", s.handleOIDCCallback)
	} else {
		r.Get("/api/auth/oidc/providers", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"providers":      []string{},
				"password_auth": s.PasswordAuthEnabled,
			})
		})
	}

	// Protected API routes
	r.Group(func(r chi.Router) {
		r.Use(s.Auth.Middleware)

		r.Get("/api/auth/me", s.handleMe)

		// Workspace routes
		r.Get("/api/workspaces", s.handleListWorkspaces)
		r.Post("/api/workspaces", s.handleCreateWorkspace)
		r.Get("/api/workspaces/quota", s.handleGetWorkspacesQuota)
		r.Get("/api/workspaces/{id}", s.handleGetWorkspace)
		r.Patch("/api/workspaces/{id}", s.handleRenameWorkspace)
		r.Delete("/api/workspaces/{id}", s.handleDeleteWorkspace)

		// Workspace member routes
		r.Get("/api/workspaces/{id}/members", s.handleListMembers)
		r.Post("/api/workspaces/{id}/members", s.handleAddMember)
		r.Put("/api/workspaces/{id}/members/{userId}", s.handleUpdateMemberRole)
		r.Delete("/api/workspaces/{id}/members/{userId}", s.handleRemoveMember)

		// Workspace LLM quota (read-only for members)
		r.Get("/api/workspaces/{id}/llm-quota", s.handleGetWorkspaceLLMQuota)

		// Workspace BYOK LLM config (owner/maintainer only)
		r.Get("/api/workspaces/{id}/llm-config", s.handleGetWorkspaceLLMConfig)
		r.Put("/api/workspaces/{id}/llm-config", s.handleSetWorkspaceLLMConfig)
		r.Delete("/api/workspaces/{id}/llm-config", s.handleDeleteWorkspaceLLMConfig)

		// ModelServer OAuth
		r.Get("/api/workspaces/{id}/modelserver/connect", s.handleModelserverConnect)
		r.Delete("/api/workspaces/{id}/modelserver/disconnect", s.handleModelserverDisconnect)
		r.Get("/api/workspaces/{id}/modelserver/status", s.handleModelserverStatus)
		r.Get("/api/auth/modelserver/callback", s.handleModelserverCallback)

		// Sandbox routes
		r.Get("/api/workspaces/{wid}/sandboxes", s.handleListSandboxes)
		r.Post("/api/workspaces/{wid}/sandboxes", s.handleCreateSandbox)
		r.Get("/api/workspaces/{wid}/defaults", s.handleGetWorkspaceDefaults)
		r.Get("/api/sandboxes/{id}", s.handleGetSandbox)
		r.Patch("/api/sandboxes/{id}", s.handleRenameSandbox)
		r.Delete("/api/sandboxes/{id}", s.handleDeleteSandbox)
		r.Post("/api/sandboxes/{id}/pause", s.handlePauseSandbox)
		r.Post("/api/sandboxes/{id}/resume", s.handleResumeSandbox)
		r.Get("/api/sandboxes/{id}/usage", s.handleSandboxUsage)
		r.Get("/api/sandboxes/{id}/traces", s.handleSandboxTraces)
		r.Get("/api/sandboxes/{id}/traces/{traceId}", s.handleTraceDetail)
		r.Get("/api/workspaces/{wid}/traces", s.handleWorkspaceTraces)
		r.Get("/api/workspaces/{wid}/traces/{traceId}", s.handleWorkspaceTraceDetail)

		// WeChat channel QR login
		r.Post("/api/sandboxes/{id}/weixin/qr-start", s.handleWeixinQRStart)
		r.Post("/api/sandboxes/{id}/weixin/qr-wait", s.handleWeixinQRWait)

		// Agent registration code generation
		r.Post("/api/workspaces/{wid}/agent-code", s.handleCreateAgentCode)

		// Admin routes
		r.Route("/api/admin", func(r chi.Router) {
			r.Use(s.requireAdmin)
			r.Get("/users", s.handleAdminListUsers)
			r.Get("/workspaces", s.handleAdminListWorkspaces)
			r.Get("/sandboxes", s.handleAdminListSandboxes)
			r.Put("/users/{id}/role", s.handleAdminUpdateUserRole)

			// Quota management
			r.Get("/quotas/defaults", s.handleAdminGetQuotaDefaults)
			r.Put("/quotas/defaults", s.handleAdminSetQuotaDefaults)
			r.Get("/users/{id}/quota", s.handleAdminGetUserQuota)
			r.Put("/users/{id}/quota", s.handleAdminSetUserQuota)
			r.Delete("/users/{id}/quota", s.handleAdminDeleteUserQuota)

			// Workspace quota management
			r.Get("/workspaces/{id}/quota", s.handleAdminGetWorkspaceQuota)
			r.Put("/workspaces/{id}/quota", s.handleAdminSetWorkspaceQuota)
			r.Delete("/workspaces/{id}/quota", s.handleAdminDeleteWorkspaceQuota)

			// Workspace LLM quota management (proxied to llmproxy)
			r.Get("/workspaces/{id}/llm-quota", s.handleAdminGetWorkspaceLLMQuota)
			r.Put("/workspaces/{id}/llm-quota", s.handleAdminSetWorkspaceLLMQuota)
			r.Delete("/workspaces/{id}/llm-quota", s.handleAdminDeleteWorkspaceLLMQuota)
		})
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
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token, _, ok := s.Auth.Login(req.Email, req.Password)
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
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Email == "" || req.Password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
		return
	}

	// Check if user already exists.
	existing, err := s.Auth.GetUserByEmail(req.Email)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		http.Error(w, "email already taken", http.StatusConflict)
		return
	}

	id := uuid.New().String()
	if err := s.Auth.Register(id, req.Email, req.Password); err != nil {
		log.Printf("register error: %v", err)
		http.Error(w, "failed to create user", http.StatusInternalServerError)
		return
	}

	// First registered user becomes admin.
	if count, err := s.DB.CountUsers(); err == nil && count == 1 {
		if err := s.DB.UpdateUserRole(id, "admin"); err != nil {
			log.Printf("failed to set first user as admin: %v", err)
		}
	}

	s.createDefaultWorkspace(id)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "email": req.Email})
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
		Name:     "agentserver-token",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
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
		"id":      user.ID,
		"email":   user.Email,
		"name":    user.Name,
		"picture": user.Picture,
		"role":    user.Role,
	})
}

// --- Response types ---

type workspaceResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type workspaceMemberResponse struct {
	UserID  string  `json:"user_id"`
	Email   string  `json:"email"`
	Role    string  `json:"role"`
	Picture *string `json:"picture,omitempty"`
}

type agentInfoResponse struct {
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version"`
	KernelArch      string `json:"kernel_arch"`
	CPUModelName    string `json:"cpu_model_name"`
	CPUCountLogical int    `json:"cpu_count_logical"`
	MemoryTotal     int64  `json:"memory_total"`
	DiskTotal       int64  `json:"disk_total"`
	DiskFree        int64  `json:"disk_free"`
	AgentVersion    string `json:"agent_version"`
	OpencodeVersion string `json:"opencode_version"`
	Workdir         string `json:"workdir"`
	UpdatedAt       string `json:"updated_at"`
}

type weixinBindingResponse struct {
	BotID   string `json:"bot_id"`
	UserID  string `json:"user_id"`
	BoundAt string `json:"bound_at"`
}

type sandboxResponse struct {
	ID              string  `json:"id"`
	ShortID         string  `json:"short_id,omitempty"`
	WorkspaceID     string  `json:"workspace_id"`
	Name            string  `json:"name"`
	Type            string  `json:"type"`
	Status          string  `json:"status"`
	OpencodeURL     string  `json:"opencode_url,omitempty"`
	OpenclawURL     string  `json:"openclaw_url,omitempty"`
	CreatedAt       string  `json:"created_at"`
	LastActivityAt  *string `json:"last_activity_at"`
	PausedAt        *string `json:"paused_at"`
	IsLocal         bool    `json:"is_local"`
	LastHeartbeatAt *string `json:"last_heartbeat_at,omitempty"`
	CPU             int     `json:"cpu,omitempty"`
	Memory          int64   `json:"memory,omitempty"`
	IdleTimeout     *int    `json:"idle_timeout,omitempty"`
	AgentInfo       *agentInfoResponse   `json:"agent_info,omitempty"`
	WeixinBindings  []weixinBindingResponse `json:"weixin_bindings,omitempty"`
}

func (s *Server) toWorkspaceResponse(ws *db.Workspace) workspaceResponse {
	return workspaceResponse{
		ID:        ws.ID,
		Name:      ws.Name,
		CreatedAt: ws.CreatedAt.Format(time.RFC3339),
		UpdatedAt: ws.UpdatedAt.Format(time.RFC3339),
	}
}

// baseDomainForRequest returns the base domain that best matches the request's
// Host header. Falls back to the primary base domain.
func (s *Server) baseDomainForRequest(r *http.Request) string {
	host := r.Host
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	for _, d := range s.BaseDomains {
		if strings.HasSuffix(host, "."+d) || host == d {
			return d
		}
	}
	if len(s.BaseDomains) > 0 {
		return s.BaseDomains[0]
	}
	return ""
}

func (s *Server) toSandboxResponse(r *http.Request, sbx *sbxstore.Sandbox, authToken string) sandboxResponse {
	resp := sandboxResponse{
		ID:          sbx.ID,
		ShortID:     sbx.ShortID,
		WorkspaceID: sbx.WorkspaceID,
		Name:        sbx.Name,
		Type:        sbx.Type,
		Status:      sbx.Status,
		CreatedAt:   sbx.CreatedAt.Format(time.RFC3339),
		IsLocal:     sbx.IsLocal,
		CPU:         sbx.CPU,
		Memory:      sbx.Memory,
		IdleTimeout: sbx.IdleTimeout,
	}
	if len(s.BaseDomains) > 0 {
		domain := s.baseDomainForRequest(r)
		subID := sbx.ShortID
		if subID == "" {
			subID = sbx.ID
		}
		switch sbx.Type {
		case "openclaw":
			resp.OpenclawURL = "https://" + s.OpenclawSubdomainPrefix + "-" + subID + "." + domain + "/auth?token=" + authToken
		case "nanoclaw":
			// NanoClaw has no Web UI — no URL to generate
		default: // "opencode"
			resp.OpencodeURL = "https://" + s.OpencodeSubdomainPrefix + "-" + subID + "." + domain + "/auth?token=" + authToken
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
	if sbx.LastHeartbeatAt != nil {
		s := sbx.LastHeartbeatAt.Format(time.RFC3339)
		resp.LastHeartbeatAt = &s
	}
	if sbx.IsLocal {
		if ai, err := s.DB.GetAgentInfo(sbx.ID); err == nil && ai != nil {
			resp.AgentInfo = &agentInfoResponse{
				Hostname:        ai.Hostname,
				OS:              ai.OS,
				Platform:        ai.Platform,
				PlatformVersion: ai.PlatformVersion,
				KernelArch:      ai.KernelArch,
				CPUModelName:    ai.CPUModelName,
				CPUCountLogical: ai.CPUCountLogical,
				MemoryTotal:     ai.MemoryTotal,
				DiskTotal:       ai.DiskTotal,
				DiskFree:        ai.DiskFree,
				AgentVersion:    ai.AgentVersion,
				OpencodeVersion: ai.OpencodeVersion,
				Workdir:         ai.Workdir,
				UpdatedAt:       ai.UpdatedAt.Format(time.RFC3339),
			}
		}
	}
	return resp
}

// attachWeixinBindings fetches and attaches weixin binding records to a sandbox response.
func (s *Server) attachWeixinBindings(resp *sandboxResponse) {
	if resp.Type != "openclaw" && resp.Type != "nanoclaw" {
		return
	}
	bindings, err := s.DB.ListWeixinBindings(resp.ID)
	if err != nil {
		log.Printf("list weixin bindings for %s: %v", resp.ID, err)
		return
	}
	for _, b := range bindings {
		resp.WeixinBindings = append(resp.WeixinBindings, weixinBindingResponse{
			BotID:   b.BotID,
			UserID:  b.UserID,
			BoundAt: b.BoundAt.Format(time.RFC3339),
		})
	}
}

// authTokenFromRequest extracts the raw auth token from the request cookie.
func authTokenFromRequest(r *http.Request) string {
	c, err := r.Cookie("agentserver-token")
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

func (s *Server) handleGetWorkspacesQuota(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	maxWs, err := s.effectiveQuota(userID)
	if err != nil {
		log.Printf("failed to get effective quota: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	current, err := s.DB.CountWorkspacesOwnedByUser(userID)
	if err != nil {
		log.Printf("failed to count workspaces: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"current": current, "max": maxWs})
}

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

	// Quota check.
	allowed, current, max, err := s.checkWorkspaceQuota(userID)
	if err != nil {
		log.Printf("failed to check workspace quota: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "quota_exceeded",
			"message": fmt.Sprintf("Workspace limit reached (%d/%d). Contact an admin to increase your quota.", current, max),
			"quota":   map[string]int{"current": current, "max": max},
		})
		return
	}

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

	// Create per-workspace K8s namespace if namespace manager is configured.
	if s.NamespaceManager != nil {
		ns, err := s.NamespaceManager.EnsureNamespace(r.Context(), id)
		if err != nil {
			log.Printf("failed to create namespace for workspace %s: %v", id, err)
			s.DB.DeleteWorkspace(id)
			http.Error(w, "failed to create workspace namespace", http.StatusInternalServerError)
			return
		}
		if err := s.DB.SetWorkspaceNamespace(id, ns); err != nil {
			log.Printf("failed to set namespace for workspace %s: %v", id, err)
			s.NamespaceManager.DeleteNamespace(r.Context(), ns)
			s.DB.DeleteWorkspace(id)
			http.Error(w, "failed to create workspace", http.StatusInternalServerError)
			return
		}
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

func (s *Server) handleRenameWorkspace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, id, "owner", "maintainer") {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := s.DB.UpdateWorkspaceName(id, req.Name); err != nil {
		log.Printf("failed to rename workspace %s: %v", id, err)
		http.Error(w, "failed to rename workspace", http.StatusInternalServerError)
		return
	}
	ws, err := s.DB.GetWorkspace(id)
	if err != nil || ws == nil {
		http.Error(w, "failed to get workspace", http.StatusInternalServerError)
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

	// Look up workspace for namespace info.
	ws, err := s.DB.GetWorkspace(id)
	if err != nil {
		log.Printf("failed to get workspace %s: %v", id, err)
		http.Error(w, "failed to delete workspace", http.StatusInternalServerError)
		return
	}

	// Resolve namespace for StopBySandboxName calls.
	var wsNamespace string
	if ws != nil && ws.K8sNamespace.Valid {
		wsNamespace = ws.K8sNamespace.String
	}

	// Stop all sandboxes in the workspace.
	sandboxes := s.Sandboxes.ListByWorkspace(id)
	for _, sbx := range sandboxes {
		if sbx.IsLocal {
			// TODO: tunnel close is now a no-op here; sandbox-proxy owns tunnel connections.
			// Tunnel will terminate when the agent's next heartbeat finds the sandbox deleted.
			if t, ok := s.TunnelRegistry.Get(sbx.ID); ok {
				t.Close()
			}
			continue
		}
		switch sbx.Status {
		case sbxstore.StatusRunning:
			s.ProcessManager.Stop(sbx.ID)
		case sbxstore.StatusPaused:
			if sbx.SandboxName != "" {
				switch mgr := s.ProcessManager.(type) {
				case interface{ StopBySandboxName(string, string) error }:
					mgr.StopBySandboxName(wsNamespace, sbx.SandboxName)
				case interface{ StopByContainerName(string) error }:
					mgr.StopByContainerName(sbx.SandboxName)
				}
			}
		}
	}

	// Delete the K8s namespace (cascades all resources).
	if s.NamespaceManager != nil && wsNamespace != "" {
		if err := s.NamespaceManager.DeleteNamespace(r.Context(), wsNamespace); err != nil {
			log.Printf("failed to delete namespace %s for workspace %s: %v", wsNamespace, id, err)
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
		email := m.UserID
		var picture *string
		if err == nil && user != nil {
			email = user.Email
			picture = user.Picture
		}
		resp = append(resp, workspaceMemberResponse{
			UserID:  m.UserID,
			Email:   email,
			Role:    m.Role,
			Picture: picture,
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
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = "developer"
	}

	user, err := s.Auth.GetUserByEmail(req.Email)
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
		UserID:  user.ID,
		Email:   user.Email,
		Role:    req.Role,
		Picture: user.Picture,
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

// handleGetWorkspaceLLMQuota returns the LLM RPD quota for a workspace (read-only for members).
func (s *Server) handleGetWorkspaceLLMQuota(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer", "developer") {
		return
	}
	s.proxyLLMProxyRequest(w, http.MethodGet, "/internal/quotas/"+wsID, nil)
}

// --- Workspace BYOK LLM config handlers ---

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:3] + "..." + key[len(key)-4:]
}

func (s *Server) handleGetWorkspaceLLMConfig(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer") {
		return
	}
	cfg, err := s.DB.GetWorkspaceLLMConfig(wsID)
	if err != nil {
		log.Printf("failed to get workspace llm config: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"configured": false})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"configured": true,
		"base_url":   cfg.BaseURL,
		"api_key":    maskAPIKey(cfg.APIKey),
		"models":     cfg.Models,
		"updated_at": cfg.UpdatedAt.Format(time.RFC3339),
	})
}

func (s *Server) handleSetWorkspaceLLMConfig(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer") {
		return
	}
	var req struct {
		BaseURL string     `json:"base_url"`
		APIKey  string     `json:"api_key"`
		Models  []db.LLMModel `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BaseURL == "" {
		http.Error(w, "base_url is required", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(req.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "base_url must be a valid http or https URL", http.StatusBadRequest)
		return
	}
	// Allow partial update: if api_key is omitted, retain the existing key.
	if req.APIKey == "" {
		existing, _ := s.DB.GetWorkspaceLLMConfig(wsID)
		if existing != nil {
			req.APIKey = existing.APIKey
		} else {
			http.Error(w, "api_key is required", http.StatusBadRequest)
			return
		}
	}
	if len(req.Models) == 0 {
		http.Error(w, "at least one model is required", http.StatusBadRequest)
		return
	}
	if len(req.Models) > 100 {
		http.Error(w, "too many models (max 100)", http.StatusBadRequest)
		return
	}
	for _, m := range req.Models {
		if m.ID == "" || m.Name == "" {
			http.Error(w, "each model must have id and name", http.StatusBadRequest)
			return
		}
	}
	if err := s.DB.SetWorkspaceLLMConfig(wsID, req.BaseURL, req.APIKey, req.Models); err != nil {
		log.Printf("failed to set workspace llm config: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (s *Server) handleDeleteWorkspaceLLMConfig(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer") {
		return
	}
	if err := s.DB.DeleteWorkspaceLLMConfig(wsID); err != nil {
		log.Printf("failed to delete workspace llm config: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Sandbox handlers ---

func (s *Server) handleGetWorkspaceDefaults(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wid")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer", "developer") {
		return
	}

	wd, err := s.effectiveWorkspaceDefaults(wsID)
	if err != nil {
		log.Printf("failed to get workspace defaults: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	currentSandboxes, err := s.DB.CountSandboxesByWorkspace(wsID)
	if err != nil {
		log.Printf("failed to count sandboxes: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"max_sandbox_cpu":    wd.MaxSandboxCPU,
		"max_sandbox_memory": wd.MaxSandboxMemory,
		"max_idle_timeout":   wd.MaxIdleTimeout,
		"max_sandboxes":      wd.MaxSandboxes,
		"current_sandboxes":  currentSandboxes,
	})
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wid")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	sandboxes := s.Sandboxes.ListByWorkspace(wsID)
	token := authTokenFromRequest(r)
	resp := make([]sandboxResponse, len(sandboxes))
	for i, sbx := range sandboxes {
		resp[i] = s.toSandboxResponse(r, sbx, token)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wid")
	if !s.requireWorkspaceRole(w, r, wsID, "owner", "maintainer", "developer") {
		return
	}

	// Quota check.
	allowed, current, max, err := s.checkSandboxQuota(wsID)
	if err != nil {
		log.Printf("failed to check sandbox quota: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !allowed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "quota_exceeded",
			"message": fmt.Sprintf("Sandbox limit reached (%d/%d). Contact an admin to increase your quota.", current, max),
			"quota":   map[string]int{"current": current, "max": max},
		})
		return
	}

	// Resolve effective workspace defaults.
	wd, err := s.effectiveWorkspaceDefaults(wsID)
	if err != nil {
		log.Printf("failed to get workspace defaults: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	cpuMillis := wd.MaxSandboxCPU   // already int millicores
	memBytes := wd.MaxSandboxMemory // already int64 bytes

	var req struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		CPU         *int   `json:"cpu"`
		Memory      *int64 `json:"memory"`
		IdleTimeout *int   `json:"idle_timeout"`
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
	if sandboxType != "opencode" && sandboxType != "openclaw" && sandboxType != "nanoclaw" {
		http.Error(w, "invalid sandbox type: must be opencode, openclaw, or nanoclaw", http.StatusBadRequest)
		return
	}

	// Override resource values if user provided them, with validation.
	if req.CPU != nil {
		if *req.CPU <= 0 || *req.CPU > wd.MaxSandboxCPU {
			http.Error(w, fmt.Sprintf("cpu must be between 1 and %d millicores", wd.MaxSandboxCPU), http.StatusBadRequest)
			return
		}
		cpuMillis = *req.CPU
	}
	if req.Memory != nil {
		if *req.Memory <= 0 || *req.Memory > wd.MaxSandboxMemory {
			http.Error(w, fmt.Sprintf("memory must be between 1 and %d bytes", wd.MaxSandboxMemory), http.StatusBadRequest)
			return
		}
		memBytes = *req.Memory
	}
	var idleTimeout *int
	if req.IdleTimeout != nil {
		if *req.IdleTimeout < 0 || (wd.MaxIdleTimeout > 0 && (*req.IdleTimeout == 0 || *req.IdleTimeout > wd.MaxIdleTimeout)) {
			http.Error(w, fmt.Sprintf("idle_timeout must be between 1 and %d seconds", wd.MaxIdleTimeout), http.StatusBadRequest)
			return
		}
		idleTimeout = req.IdleTimeout
	}

	// Check workspace resource budget.
	budgetOk, err := s.checkWorkspaceResourceBudget(wsID, cpuMillis, memBytes)
	if err != nil {
		log.Printf("failed to check workspace resource budget: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !budgetOk {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "resource_budget_exceeded",
			"message": "Workspace resource budget exceeded. Delete or pause existing sandboxes to free resources.",
		})
		return
	}

	// Look up workspace namespace.
	ws, err := s.DB.GetWorkspace(wsID)
	if err != nil || ws == nil {
		log.Printf("failed to get workspace %s: %v", wsID, err)
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}
	var wsNamespace string
	if ws.K8sNamespace.Valid {
		wsNamespace = ws.K8sNamespace.String
	}

	// Ensure workspace drive exists.
	workspaceVolumes, err := s.DriveManager.EnsureDrive(r.Context(), wsID, wsNamespace)
	if err != nil {
		log.Printf("failed to ensure workspace drive for %s: %v", wsID, err)
		// Non-fatal: sandbox can still work without workspace drive.
	}

	id := uuid.New().String()
	sandboxName := "agent-sandbox-" + shortID(id)

	// Look up modelserver connection and BYOK config for this workspace.
	msConn, _ := s.DB.GetModelserverConnection(wsID)
	byokCfg, err := s.DB.GetWorkspaceLLMConfig(wsID)
	if err != nil {
		log.Printf("failed to get BYOK config for workspace %s: %v", wsID, err)
		byokCfg = nil
	}

	// Generate auth credentials based on sandbox type.
	var opencodeToken, openclawToken string
	proxyToken := generatePassword()
	switch sandboxType {
	case "openclaw":
		openclawToken = generatePassword()
	case "nanoclaw":
		// NanoClaw uses a bridge secret instead of openclaw/opencode tokens.
		// The bridge secret is stored separately after sandbox creation.
	default: // "opencode"
		opencodeToken = generatePassword()
	}

	// Generate a short ID for subdomain routing (retry on collision).
	sid := shortid.Generate()
	var sbx *sbxstore.Sandbox
	var createErr error
	for attempts := 0; attempts < 3; attempts++ {
		sbx, createErr = s.Sandboxes.Create(id, wsID, req.Name, sandboxType, sandboxName, opencodeToken, proxyToken, openclawToken, sid, cpuMillis, memBytes, idleTimeout)
		if createErr == nil {
			break
		}
		sid = shortid.Generate()
	}
	if createErr != nil {
		log.Printf("failed to create sandbox: %v", createErr)
		http.Error(w, "failed to create sandbox", http.StatusInternalServerError)
		return
	}

	// Generate and store bridge secret for nanoclaw sandboxes.
	if sandboxType == "nanoclaw" {
		bridgeSecret := generatePassword()
		if err := s.DB.UpdateSandboxNanoclawBridgeSecret(id, bridgeSecret); err != nil {
			log.Printf("failed to store nanoclaw bridge secret: %v", err)
		}
		sbx.NanoclawBridgeSecret = bridgeSecret
	}

	// Build start options.
	startOpts := process.StartOptions{
		Namespace:        wsNamespace,
		WorkspaceVolumes: workspaceVolumes,
		OpencodeToken:    opencodeToken,
		ProxyToken:       proxyToken,
		SandboxType:      sandboxType,
		OpenclawToken:    openclawToken,
		CPU:              cpuMillis,
		Memory:           memBytes,
	}
	if sandboxType == "nanoclaw" {
		startOpts.NanoclawBridgeSecret = sbx.NanoclawBridgeSecret
	}
	// Priority: modelserver > BYOK > platform default
	if msConn != nil {
		// Modelserver connection: sandbox routes through llmproxy (no BYOK injection)
		startOpts.CustomModels = make([]process.LLMModel, len(msConn.Models))
		for i, m := range msConn.Models {
			startOpts.CustomModels[i] = process.LLMModel{ID: m.ID, Name: m.Name}
		}
	} else if byokCfg != nil {
		startOpts.BYOKBaseURL = byokCfg.BaseURL
		startOpts.BYOKAPIKey = byokCfg.APIKey
		startOpts.BYOKModels = make([]process.LLMModel, len(byokCfg.Models))
		for i, m := range byokCfg.Models {
			startOpts.BYOKModels[i] = process.LLMModel{ID: m.ID, Name: m.Name}
		}
	}

	// Start container asynchronously.
	go func() {
		var podIP string
		// Use StartContainerWithIP if available (K8s backend) to get the pod IP.
		if sc, ok := s.ProcessManager.(interface {
			StartContainerWithIP(string, process.StartOptions) (string, error)
		}); ok {
			var err error
			podIP, err = sc.StartContainerWithIP(id, startOpts)
			if err != nil {
				log.Printf("failed to start container for sandbox %s: %v", id, err)
				s.Sandboxes.Delete(id)
				return
			}
		} else {
			if err := s.ProcessManager.StartContainer(id, startOpts); err != nil {
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
	json.NewEncoder(w).Encode(s.toSandboxResponse(r, sbx, authTokenFromRequest(r)))
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
	resp := s.toSandboxResponse(r, sbx, authTokenFromRequest(r))
	s.attachWeixinBindings(&resp)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleRenameSandbox(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := s.DB.UpdateSandboxName(id, req.Name); err != nil {
		log.Printf("failed to rename sandbox %s: %v", id, err)
		http.Error(w, "failed to rename sandbox", http.StatusInternalServerError)
		return
	}
	sbx.Name = req.Name
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.toSandboxResponse(r, sbx, authTokenFromRequest(r)))
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
	if sbx.IsLocal {
		// TODO: tunnel close is now a no-op here; sandbox-proxy owns tunnel connections.
		// Tunnel will terminate when the agent's next heartbeat finds the sandbox deleted.
		if t, ok := s.TunnelRegistry.Get(id); ok {
			t.Close()
		}
	} else {
		switch sbx.Status {
		case sbxstore.StatusRunning:
			s.ProcessManager.Stop(id)
		case sbxstore.StatusPaused:
			if sbx.SandboxName != "" {
				// Look up workspace namespace for sandbox deletion.
				var sbxNs string
				if ws, err := s.DB.GetWorkspace(sbx.WorkspaceID); err == nil && ws != nil && ws.K8sNamespace.Valid {
					sbxNs = ws.K8sNamespace.String
				}
				switch mgr := s.ProcessManager.(type) {
				case interface{ StopBySandboxName(string, string) error }:
					mgr.StopBySandboxName(sbxNs, sbx.SandboxName)
				case interface{ StopByContainerName(string) error }:
					mgr.StopByContainerName(sbx.SandboxName)
				}
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

	if sbx.IsLocal {
		http.Error(w, "local sandboxes cannot be paused", http.StatusBadRequest)
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

	if sbx.IsLocal {
		http.Error(w, "local sandboxes cannot be resumed from server", http.StatusBadRequest)
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

func (s *Server) handleSandboxUsage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if s.LLMProxyURL == "" {
		http.Error(w, "llmproxy not configured", http.StatusServiceUnavailable)
		return
	}
	proxyURL := s.LLMProxyURL + "/internal/usage?sandbox_id=" + id
	s.proxyLLMRequest(w, proxyURL)
}

func (s *Server) handleSandboxTraces(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if s.LLMProxyURL == "" {
		http.Error(w, "llmproxy not configured", http.StatusServiceUnavailable)
		return
	}
	proxyURL := s.LLMProxyURL + "/internal/traces?sandbox_id=" + id
	if limit := r.URL.Query().Get("limit"); limit != "" {
		proxyURL += "&limit=" + limit
	}
	if offset := r.URL.Query().Get("offset"); offset != "" {
		proxyURL += "&offset=" + offset
	}
	s.proxyLLMRequest(w, proxyURL)
}

func (s *Server) handleTraceDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if s.LLMProxyURL == "" {
		http.Error(w, "llmproxy not configured", http.StatusServiceUnavailable)
		return
	}
	traceId := chi.URLParam(r, "traceId")
	proxyURL := s.LLMProxyURL + "/internal/traces/" + traceId
	s.proxyLLMRequest(w, proxyURL)
}

func (s *Server) handleWorkspaceTraces(w http.ResponseWriter, r *http.Request) {
	wid := chi.URLParam(r, "wid")
	if _, ok := s.requireWorkspaceMember(w, r, wid); !ok {
		return
	}
	if s.LLMProxyURL == "" {
		http.Error(w, "llmproxy not configured", http.StatusServiceUnavailable)
		return
	}
	proxyURL := s.LLMProxyURL + "/internal/traces?workspace_id=" + wid
	if limit := r.URL.Query().Get("limit"); limit != "" {
		proxyURL += "&limit=" + limit
	}
	if offset := r.URL.Query().Get("offset"); offset != "" {
		proxyURL += "&offset=" + offset
	}
	s.proxyLLMRequest(w, proxyURL)
}

func (s *Server) handleWorkspaceTraceDetail(w http.ResponseWriter, r *http.Request) {
	wid := chi.URLParam(r, "wid")
	if _, ok := s.requireWorkspaceMember(w, r, wid); !ok {
		return
	}
	if s.LLMProxyURL == "" {
		http.Error(w, "llmproxy not configured", http.StatusServiceUnavailable)
		return
	}
	traceId := chi.URLParam(r, "traceId")
	proxyURL := s.LLMProxyURL + "/internal/traces/" + traceId
	s.proxyLLMRequest(w, proxyURL)
}

func (s *Server) proxyLLMRequest(w http.ResponseWriter, url string) {
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("llmproxy request failed: %v", err)
		http.Error(w, "llmproxy unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
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

// ---------------------------------------------------------------------------
// WeChat channel QR login
// ---------------------------------------------------------------------------

func (s *Server) handleWeixinQRStart(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if sbx.Type != "openclaw" && sbx.Type != "nanoclaw" {
		http.Error(w, "weixin login is only available for openclaw and nanoclaw sandboxes", http.StatusBadRequest)
		return
	}
	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	session, err := weixin.StartLogin(r.Context(), weixin.DefaultAPIBaseURL)
	if err != nil {
		log.Printf("weixin qr-start: %v", err)
		http.Error(w, "failed to start weixin login", http.StatusBadGateway)
		return
	}
	weixin.SetSession(id, session)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"qrcode_url": session.QRCodeURL,
		"message":    "Scan the QR code with WeChat",
	})
}

// execCommander is implemented by sandbox managers that support one-shot exec.
type execCommander interface {
	ExecSimple(ctx context.Context, sandboxID string, command []string) (string, error)
}

func (s *Server) handleWeixinQRWait(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if sbx.Type != "openclaw" && sbx.Type != "nanoclaw" {
		http.Error(w, "weixin login is only available for openclaw and nanoclaw sandboxes", http.StatusBadRequest)
		return
	}
	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	session := weixin.GetSession(id)
	if session == nil {
		http.Error(w, "no active weixin login session", http.StatusBadRequest)
		return
	}

	result, err := weixin.PollLoginStatus(r.Context(), weixin.DefaultAPIBaseURL, session.QRCode)
	if err != nil {
		log.Printf("weixin qr-wait: poll error: %v", err)
		http.Error(w, "poll failed", http.StatusBadGateway)
		return
	}

	switch result.Status {
	case "confirmed":
		weixin.TakeSession(id) // atomic get+delete to prevent duplicate saves
		if err := s.saveWeixinCredentials(r.Context(), id, result); err != nil {
			log.Printf("weixin qr-wait: save credentials: %v", err)
			http.Error(w, "login succeeded but failed to save credentials", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": true,
			"status":    "confirmed",
			"message":   "WeChat connected successfully",
			"bot_id":    normalizeAccountID(result.BotID),
			"user_id":   result.UserID,
		})

	case "expired":
		// Auto-refresh QR code.
		newSession, err := weixin.StartLogin(r.Context(), weixin.DefaultAPIBaseURL)
		if err != nil {
			weixin.ClearSession(id)
			http.Error(w, "QR code expired and refresh failed", http.StatusBadGateway)
			return
		}
		weixin.SetSession(id, newSession)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected":  false,
			"status":     "expired",
			"message":    "QR code expired, new code generated",
			"qrcode_url": newSession.QRCodeURL,
		})

	default: // "wait", "scaned"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": false,
			"status":    result.Status,
			"message":   statusMessage(result.Status),
		})
	}
}

func statusMessage(status string) string {
	switch status {
	case "scaned":
		return "QR code scanned, confirm on WeChat"
	default:
		return "Waiting for QR code scan"
	}
}

func (s *Server) saveWeixinCredentials(ctx context.Context, sandboxID string, result *weixin.StatusResult) error {
	accountID := normalizeAccountID(result.BotID)
	if accountID == "" {
		return fmt.Errorf("empty bot ID from ilink response")
	}

	// For nanoclaw: store credentials in DB (bridge mode).
	sbx, ok := s.Sandboxes.Get(sandboxID)
	if ok && sbx.Type == "nanoclaw" {
		baseURL := result.BaseURL
		if baseURL == "" {
			baseURL = weixin.DefaultAPIBaseURL
		}
		// Save binding record first.
		if dbErr := s.DB.CreateWeixinBinding(sandboxID, accountID, result.UserID); dbErr != nil {
			return fmt.Errorf("save binding: %w", dbErr)
		}
		// Store bot credentials for bridge messaging.
		if dbErr := s.DB.SaveBotCredentials(sandboxID, accountID, result.Token, baseURL); dbErr != nil {
			return fmt.Errorf("save bot credentials: %w", dbErr)
		}
		// TODO: Register webhook with iLink for inbound message delivery
		// (blocked on iLink API investigation)
		return nil
	}

	// Existing openclaw logic: write credentials into pod filesystem.
	commander, ok := s.ProcessManager.(execCommander)
	if !ok {
		return fmt.Errorf("process manager does not support exec")
	}

	baseURL := result.BaseURL
	if baseURL == "" {
		baseURL = weixin.DefaultAPIBaseURL
	}

	// Marshal credentials as JSON, then base64-encode to avoid any shell injection.
	credJSON, err := json.Marshal(map[string]string{
		"token":   result.Token,
		"baseUrl": baseURL,
		"savedAt": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	indexJSON, err := json.Marshal([]string{accountID})
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}

	b64Cred := base64Encode(credJSON)
	b64Index := base64Encode(indexJSON)

	// Decode base64 inside the pod to write safe JSON files, then poke the
	// config file so the gateway's chokidar watcher triggers a channel reload.
	// (Node.js terminates on SIGHUP, so we cannot use kill -HUP 1.)
	script := fmt.Sprintf(
		`mkdir -p ~/.openclaw/openclaw-weixin/accounts && `+
			`echo %s | base64 -d > ~/.openclaw/openclaw-weixin/accounts/%s.json && `+
			`echo %s | base64 -d > ~/.openclaw/openclaw-weixin/accounts.json && `+
			`node -e "`+
			`const fs=require('fs'),p=require('os').homedir()+'/.openclaw/openclaw.json';`+
			`const c=JSON.parse(fs.readFileSync(p,'utf8'));`+
			`c.channels=c.channels||{};`+
			`c.channels['openclaw-weixin']=c.channels['openclaw-weixin']||{};`+
			`c.channels['openclaw-weixin']._accountsUpdatedAt=Date.now();`+
			`fs.writeFileSync(p,JSON.stringify(c,null,2))"`,
		b64Cred, accountID, b64Index,
	)

	_, err = commander.ExecSimple(ctx, sandboxID, []string{"sh", "-c", script})
	if err != nil {
		return err
	}

	// Persist binding record to DB (non-fatal if it fails).
	if dbErr := s.DB.CreateWeixinBinding(sandboxID, accountID, result.UserID); dbErr != nil {
		log.Printf("weixin: failed to save binding record: %v", dbErr)
	}
	return nil
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// normalizeAccountID converts a raw ilink bot ID (e.g. "abc@im.bot") to a
// filesystem-safe key (e.g. "abc-im-bot"), matching the plugin's normalizeAccountId.
func normalizeAccountID(raw string) string {
	var out []byte
	for _, c := range []byte(raw) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
