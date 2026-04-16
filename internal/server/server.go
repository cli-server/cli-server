package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/bridge"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/namespace"
	"github.com/agentserver/agentserver/internal/process"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/shortid"
	"github.com/agentserver/agentserver/internal/storage"
	"github.com/agentserver/agentserver/internal/tunnel"
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
	OpenclawSubdomainPrefix    string // e.g. "claw" — subdomain: claw-{id}.{baseDomain}
	ClaudeCodeSubdomainPrefix  string // e.g. "claude" — subdomain: claude-{id}.{baseDomain}
	PasswordAuthEnabled      bool   // when false, /api/auth/login and /api/auth/register are not registered
	LLMProxyURL              string // base URL for the llmproxy service (e.g. "http://agentserver-llmproxy:8081")

	// IMBridgeURL is the base URL of the standalone imbridge service
	// (e.g. "http://agentserver-imbridge:8083"). When set, IM API routes
	// are reverse-proxied to the imbridge service.
	IMBridgeURL string

	// ModelServer OAuth
	ModelserverOAuthClientID      string
	ModelserverOAuthClientSecret  string
	ModelserverOAuthAuthURL       string
	ModelserverOAuthTokenURL      string
	ModelserverOAuthIntrospectURL string
	ModelserverOAuthRedirectURI   string
	ModelserverProxyURL           string
	DatabaseURL                  string // PostgreSQL connection URL (needed for Matrix E2EE crypto DB)

	// Hydra OAuth2 (for agent Device Flow)
	HydraClient    *auth.HydraClient
	HydraPublicURL string // internal URL for reverse proxy (e.g. "http://hydra-public:4444")

	// BridgeHandler provides CCR V2-compatible bridge API for agent sessions.
	BridgeHandler *bridge.Handler

	// CCBrokerURL is the base URL of the cc-broker service for stateless
	// Claude Code sessions (e.g. "http://cc-broker:8090").
	CCBrokerURL string

	// Credential proxy
	EncryptionKey    []byte // AES-256 key for credential_bindings auth_blob
	CredproxyPublicURL string // URL sandboxes use to reach credentialproxy

	// In-memory pending device code flows (OIDC credential creation).
	deviceFlows   map[string]*pendingDeviceFlow
	deviceFlowsMu sync.Mutex
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
	claudecodePrefix := os.Getenv("CLAUDECODE_SUBDOMAIN_PREFIX")
	if claudecodePrefix == "" {
		claudecodePrefix = "claude"
	}

	s := &Server{
		Auth:                      a,
		OIDC:                      oidcMgr,
		DB:                        database,
		Sandboxes:                 sandboxStore,
		ProcessManager:            processManager,
		DriveManager:              driveManager,
		NamespaceManager:          nsMgr,
		TunnelRegistry:            tunnelReg,
		StaticFS:                  staticFS,
		BaseDomains:               baseDomains,
		OpencodeSubdomainPrefix:   opcodePrefix,
		OpenclawSubdomainPrefix:   openclawPrefix,
		ClaudeCodeSubdomainPrefix: claudecodePrefix,
		PasswordAuthEnabled:       passwordAuthEnabled,
		deviceFlows:               make(map[string]*pendingDeviceFlow),
	}
	if s.OIDC != nil {
		s.OIDC.OnUserCreated = s.createDefaultWorkspace
	}
	// Background sweep for expired device code flows (OIDC).
	go s.sweepExpiredDeviceFlows()
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

	// IM bridge routes: proxy to standalone imbridge service when configured.
	if s.IMBridgeURL != "" {
		imbridgeProxy := newReverseProxy(s.IMBridgeURL)
		// Internal API for NanoClaw pods to send IM replies (auth via bridge secret).
		r.Post("/api/internal/nanoclaw/{id}/im/send", imbridgeProxy)
		r.Post("/api/internal/nanoclaw/{id}/weixin/send", imbridgeProxy) // legacy alias
	}

	// Agent registration (auth via OAuth Bearer token).
	r.Post("/api/agent/register", s.handleAgentRegister)

	// Hydra login/consent provider endpoints (no auth required — Hydra redirects here).
	if s.HydraClient != nil {
		r.Get("/api/oauth2/login", s.handleOAuthLogin)
		r.Post("/api/oauth2/login", s.handleOAuthLoginSubmit)
		r.Get("/api/oauth2/consent", s.handleOAuthConsent)
		r.Post("/api/oauth2/consent", s.handleOAuthConsentSubmit)
		r.Post("/api/oauth2/device/accept", s.handleOAuthDeviceAccept)
	}

	// Reverse proxy Hydra public endpoints so CLI only needs the agentserver URL.
	// Rewrites /api/oauth2/* → /oauth2/* on the Hydra side.
	if s.HydraPublicURL != "" {
		r.Post("/api/oauth2/device/auth", s.hydraProxyRewrite("/oauth2/device/auth"))
		r.Post("/api/oauth2/token", s.hydraProxyRewrite("/oauth2/token"))
		// Hydra's verification_uri is always issuer + /oauth2/device/verify (hardcoded
		// in fositex/config.go:240). This is the entry point for the browser flow —
		// Hydra processes user_code then redirects to URLS_DEVICE_VERIFICATION.
		hydraPassthrough := newReverseProxy(s.HydraPublicURL)
		r.Get("/oauth2/device/verify", hydraPassthrough)
		r.Post("/oauth2/device/verify", hydraPassthrough)
	}

	// Agent card registration (auth via proxy_token).
	r.Post("/api/agent/discovery/cards", s.handleRegisterAgentCard)

	// Task polling and status updates for workers (auth via proxy_token).
	r.Get("/api/agent/tasks/poll", s.handlePollTasks)
	r.Put("/api/agent/tasks/{id}/status", s.handleUpdateTaskStatus)

	// Agent mailbox (auth via proxy_token).
	r.Post("/api/agent/mailbox/send", s.handleSendMessage)
	r.Get("/api/agent/mailbox/inbox", s.handleReadInbox)

	// Agent-facing discovery and task routes (auth via proxy_token).
	// These mirror the cookie-auth routes below but accept Bearer token
	// so MCP bridge inside sandbox pods can call them.
	r.Get("/api/agent/discovery/agents", s.handleAgentDiscoverAgents)
	r.Post("/api/agent/tasks", s.handleAgentCreateTask)
	r.Get("/api/agent/tasks/{id}", s.handleAgentGetTask)

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

		// Credential binding routes
		r.Get("/api/workspaces/{id}/credentials/{kind}", s.handleListCredentialBindings)
		r.Post("/api/workspaces/{id}/credentials/{kind}", s.handleCreateCredentialBinding)
		r.Patch("/api/workspaces/{id}/credentials/{kind}/{bindingId}", s.handlePatchCredentialBinding)
		r.Delete("/api/workspaces/{id}/credentials/{kind}/{bindingId}", s.handleDeleteCredentialBinding)
		r.Post("/api/workspaces/{id}/credentials/{kind}/{bindingId}/set-default", s.handleSetDefaultCredentialBinding)
		r.Post("/api/workspaces/{id}/credentials/{kind}/{bindingId}/device-complete", s.handleDeviceCodeComplete)

		// IM routes: proxy to standalone imbridge service.
		if s.IMBridgeURL != "" {
			imbridgeProxy := newReverseProxy(s.IMBridgeURL)
			// Workspace IM channel management
			r.Get("/api/workspaces/{id}/im/channels", imbridgeProxy)
			r.Delete("/api/workspaces/{id}/im/channels/{channelId}", imbridgeProxy)
			r.Patch("/api/workspaces/{id}/im/channels/{channelId}", imbridgeProxy)
			r.Post("/api/workspaces/{id}/im/weixin/qr-start", imbridgeProxy)
			r.Post("/api/workspaces/{id}/im/weixin/qr-wait", imbridgeProxy)
			r.Post("/api/workspaces/{id}/im/telegram/configure", imbridgeProxy)
			r.Post("/api/workspaces/{id}/im/matrix/configure", imbridgeProxy)
			// Sandbox IM channel binding
			r.Post("/api/sandboxes/{id}/im/bind", imbridgeProxy)
			r.Delete("/api/sandboxes/{id}/im/bind", imbridgeProxy)
			// Legacy sandbox-level IM routes
			r.Post("/api/sandboxes/{id}/im/weixin/qr-start", imbridgeProxy)
			r.Post("/api/sandboxes/{id}/im/weixin/qr-wait", imbridgeProxy)
			r.Post("/api/sandboxes/{id}/im/telegram/configure", imbridgeProxy)
			r.Delete("/api/sandboxes/{id}/im/telegram", imbridgeProxy)
			r.Post("/api/sandboxes/{id}/im/matrix/configure", imbridgeProxy)
			r.Delete("/api/sandboxes/{id}/im/matrix", imbridgeProxy)
			r.Get("/api/sandboxes/{id}/im/bindings", imbridgeProxy)
			r.Post("/api/sandboxes/{id}/weixin/qr-start", imbridgeProxy)
			r.Post("/api/sandboxes/{id}/weixin/qr-wait", imbridgeProxy)
		}

		// IM inbound handler (stateless CC sessions via cc-broker)
		r.Post("/api/workspaces/{wid}/im/inbound", s.handleIMInbound)

		// Agent discovery
		r.Get("/api/workspaces/{wid}/agents", s.handleListAgentCards)
		r.Get("/api/agents/{sandboxId}", s.handleGetAgentCard)

		// Agent tasks
		r.Post("/api/workspaces/{wid}/tasks", s.handleCreateTask)
		r.Get("/api/workspaces/{wid}/tasks", s.handleListTasks)
		r.Get("/api/tasks/{id}", s.handleGetTask)
		r.Post("/api/tasks/{id}/cancel", s.handleCancelTask)
		r.Get("/api/tasks/{id}/stream", s.handleTaskStream)

		// Agent interaction audit trail
		r.Get("/api/workspaces/{wid}/agent-interactions", s.handleListInteractions)

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

	// Bridge API (CCR V2 compatible)
	if s.BridgeHandler != nil {
		r.Route("/v1/agent", func(r chi.Router) {
			// Session lifecycle: proxy_token or cookie auth
			r.Group(func(r chi.Router) {
				r.Use(s.BridgeHandler.AgentOrUserAuthMiddleware(s.Auth.Middleware))
				r.Post("/sessions", s.BridgeHandler.HandleCreateSession)
				r.Post("/sessions/{sessionId}/bridge", s.BridgeHandler.HandleBridge)
				r.Post("/sessions/{sessionId}/archive", s.BridgeHandler.HandleArchive)
			})
			// Worker endpoints: JWT auth
			r.Route("/sessions/{sessionId}", func(r chi.Router) {
				r.Use(s.BridgeHandler.WorkerAuthMiddleware)
				r.Get("/worker/events/stream", s.BridgeHandler.HandleWorkerEventStream)
				r.Post("/worker/events", s.BridgeHandler.HandleWorkerEvents)
				r.Post("/worker/internal-events", s.BridgeHandler.HandleWorkerInternalEvents)
				r.Post("/worker/events/delivery", s.BridgeHandler.HandleWorkerDelivery)
				r.Put("/worker", s.BridgeHandler.HandleWorkerState)
				r.Post("/worker/heartbeat", s.BridgeHandler.HandleWorkerHeartbeat)
				r.Get("/worker", s.BridgeHandler.HandleGetWorker)
				r.Get("/worker/internal-events", s.BridgeHandler.HandleGetInternalEvents)
			})
		})
	}

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

type imBindingResponse struct {
	Provider string `json:"provider"`
	BotID    string `json:"bot_id"`
	UserID   string `json:"user_id,omitempty"`
	BoundAt  string `json:"bound_at"`
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
	ClaudeCodeURL   string  `json:"claudecode_url,omitempty"`
	CustomURL       string  `json:"custom_url,omitempty"`
	CreatedAt       string  `json:"created_at"`
	LastActivityAt  *string `json:"last_activity_at"`
	PausedAt        *string `json:"paused_at"`
	IsLocal         bool    `json:"is_local"`
	LastHeartbeatAt *string `json:"last_heartbeat_at,omitempty"`
	CPU             int     `json:"cpu,omitempty"`
	Memory          int64   `json:"memory,omitempty"`
	IdleTimeout     *int    `json:"idle_timeout,omitempty"`
	AgentInfo       *agentInfoResponse     `json:"agent_info,omitempty"`
	WeixinBindings  []imBindingResponse    `json:"weixin_bindings,omitempty"`
	IMBindings      []imBindingResponse    `json:"im_bindings,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
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
		case "claudecode":
			resp.ClaudeCodeURL = "https://" + s.ClaudeCodeSubdomainPrefix + "-" + subID + "." + domain + "/auth?token=" + authToken
		case "custom":
			// Custom agents use the opencode subdomain prefix (code-{id}.domain)
			// but skip SPA fallback in the proxy handler.
			resp.CustomURL = "https://" + s.OpencodeSubdomainPrefix + "-" + subID + "." + domain + "/auth?token=" + authToken
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
	if len(sbx.Metadata) > 0 {
		resp.Metadata = sbx.Metadata
	}
	return resp
}

// attachIMBindings fetches and attaches IM channel records to a sandbox response.
func (s *Server) attachIMBindings(resp *sandboxResponse) {
	if resp.Type != "openclaw" && resp.Type != "nanoclaw" {
		return
	}
	// Return only the channel bound to THIS sandbox.
	ch, err := s.DB.GetIMChannelForSandbox(resp.ID)
	if err != nil {
		return
	}
	entry := imBindingResponse{
		Provider: ch.Provider,
		BotID:    ch.BotID,
		UserID:   ch.UserID,
		BoundAt:  ch.BoundAt.Format(time.RFC3339),
	}
	resp.IMBindings = append(resp.IMBindings, entry)
	if ch.Provider == "weixin" {
		resp.WeixinBindings = append(resp.WeixinBindings, entry)
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
		s.attachIMBindings(&resp[i])
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
		Name          string                 `json:"name"`
		Type          string                 `json:"type"`
		CPU           *int                   `json:"cpu"`
		Memory        *int64                 `json:"memory"`
		IdleTimeout   *int                   `json:"idle_timeout"`
		Metadata      map[string]interface{} `json:"metadata"`
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
	if sandboxType != "opencode" && sandboxType != "openclaw" && sandboxType != "nanoclaw" && sandboxType != "claudecode" {
		http.Error(w, "invalid sandbox type: must be opencode, openclaw, nanoclaw, or claudecode", http.StatusBadRequest)
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
	case "claudecode":
		// Claude Code only uses proxyToken (as ANTHROPIC_API_KEY).
	default: // "opencode"
		opencodeToken = generatePassword()
	}

	// Generate a short ID for subdomain routing (retry on collision).
	sid := shortid.Generate()
	var sbx *sbxstore.Sandbox
	var createErr error
	for attempts := 0; attempts < 3; attempts++ {
		sbx, createErr = s.Sandboxes.Create(id, wsID, req.Name, sandboxType, sandboxName, opencodeToken, proxyToken, openclawToken, sid, cpuMillis, memBytes, idleTimeout, req.Metadata)
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
		startOpts.SandboxID = id
		startOpts.WorkspaceID = wsID
		startOpts.AssistantName = sbx.MetadataString("assistant_name")
	}
	if sandboxType == "claudecode" {
		startOpts.SandboxID = id
		startOpts.WorkspaceID = wsID
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
	s.attachIMBindings(&resp)
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

	// Unbind sandbox from its IM channel (poller keeps running at channel level).
	if sbx.Type == "nanoclaw" {
		if err := s.DB.UnbindSandboxFromChannel(id); err != nil {
			log.Printf("failed to unbind sandbox %s from IM channel: %v", id, err)
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

	// Note: we do NOT unbind the sandbox from its IM channel on pause.
	// The poller skips forwarding when the sandbox is not running (checks status='running' and pod_ip != '').
	// The binding is preserved so messages resume flowing when the sandbox is resumed.

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

		// Restart IM bridge pollers for nanoclaw sandboxes after resume.
		// The Pod has a new IP; notify imbridge to restart pollers.
		sbxNow, ok := s.Sandboxes.Get(id)
		if ok && sbxNow.Type == "nanoclaw" && s.IMBridgeURL != "" {
			go s.notifyIMBridgePollerRestore(id)
		}

		// WeChat credentials for openclaw sandboxes persist on PVC across
		// pause/resume, and the config merge preserves plugin metadata.
		// No re-injection needed.
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

// notifyIMBridgePollerRestore sends a fire-and-forget notification to the
// imbridge service to restart pollers for a sandbox (e.g. after resume).
func (s *Server) notifyIMBridgePollerRestore(sandboxID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reqURL := s.IMBridgeURL + "/api/internal/imbridge/pollers/" + sandboxID + "/restore"
	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, nil)
	if err != nil {
		log.Printf("imbridge: failed to build restore request for %s: %v", sandboxID, err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("imbridge: failed to notify poller restore for %s: %v", sandboxID, err)
		return
	}
	resp.Body.Close()
}

// newReverseProxy creates an HTTP handler that proxies requests to the given base URL.
func newReverseProxy(baseURL string) http.HandlerFunc {
	target, err := url.Parse(baseURL)
	if err != nil {
		log.Fatalf("invalid proxy target URL %q: %v", baseURL, err)
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
	}
	return func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}
}

// hydraProxyRewrite returns a handler that proxies to Hydra with a rewritten path.
// URL is parsed once at init time; invalid URL causes a fatal startup error.
func (s *Server) hydraProxyRewrite(targetPath string) http.HandlerFunc {
	target, err := url.Parse(s.HydraPublicURL)
	if err != nil {
		log.Fatalf("invalid Hydra public URL %q: %v", s.HydraPublicURL, err)
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = targetPath
			req.Host = target.Host
		},
	}
	return func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}
}
