package imbridgesvc

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/imbridge"
	"github.com/agentserver/agentserver/internal/sbxstore"
)

// Server is the standalone imbridge HTTP service.
type Server struct {
	db        *db.DB
	auth      *auth.Auth
	sandboxes *sbxstore.Store
	bridge    *imbridge.Bridge
}

// NewServer creates a new imbridge service.
func NewServer(database *db.DB, authSvc *auth.Auth, sandboxStore *sbxstore.Store, bridge *imbridge.Bridge) *Server {
	return &Server{
		db:        database,
		auth:      authSvc,
		sandboxes: sandboxStore,
		bridge:    bridge,
	}
}

// Routes returns the HTTP handler for all imbridge endpoints.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Health endpoint (K8s probes).
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Internal API: NanoClaw pods send outbound IM messages (auth via bridge secret).
	r.Post("/api/internal/nanoclaw/{id}/im/send", s.handleNanoclawIMSend)
	r.Post("/api/internal/nanoclaw/{id}/weixin/send", s.handleNanoclawIMSend) // legacy alias

	// Internal API: agentserver notifies imbridge of lifecycle events.
	r.Post("/api/internal/imbridge/pollers/{sandboxId}/restore", s.handleRestorePollers)
	r.Post("/api/internal/imbridge/pollers/{channelId}/stop", s.handleStopPoller)

	// Internal API: agentserver sends IM replies for stateless CC sessions
	// (auth via X-Internal-Secret shared secret).
	r.Post("/api/internal/imbridge/send", s.handleImbridgeDirectSend)

	// Authenticated API routes (cookie auth).
	r.Group(func(r chi.Router) {
		r.Use(s.auth.Middleware)

		// Workspace IM channel management.
		r.Get("/api/workspaces/{id}/im/channels", s.handleListWorkspaceIMChannels)
		r.Delete("/api/workspaces/{id}/im/channels/{channelId}", s.handleDeleteWorkspaceIMChannel)
		r.Patch("/api/workspaces/{id}/im/channels/{channelId}", s.handleUpdateWorkspaceIMChannel)
		r.Post("/api/workspaces/{id}/im/weixin/qr-start", s.handleWorkspaceWeixinQRStart)
		r.Post("/api/workspaces/{id}/im/weixin/qr-wait", s.handleWorkspaceWeixinQRWait)
		r.Post("/api/workspaces/{id}/im/telegram/configure", s.handleWorkspaceTelegramConfigure)
		r.Post("/api/workspaces/{id}/im/matrix/configure", s.handleWorkspaceMatrixConfigure)

		// Sandbox IM channel binding.
		r.Post("/api/sandboxes/{id}/im/bind", s.handleBindSandboxToChannel)
		r.Delete("/api/sandboxes/{id}/im/bind", s.handleUnbindSandboxFromChannel)

		// Legacy sandbox-level IM routes.
		r.Post("/api/sandboxes/{id}/im/weixin/qr-start", s.handleIMWeixinQRStart)
		r.Post("/api/sandboxes/{id}/im/weixin/qr-wait", s.handleIMWeixinQRWait)
		r.Post("/api/sandboxes/{id}/im/telegram/configure", s.handleIMTelegramConfigure)
		r.Delete("/api/sandboxes/{id}/im/telegram", s.handleIMTelegramDisconnect)
		r.Post("/api/sandboxes/{id}/im/matrix/configure", s.handleIMMatrixConfigure)
		r.Delete("/api/sandboxes/{id}/im/matrix", s.handleIMMatrixDisconnect)
		r.Get("/api/sandboxes/{id}/im/bindings", s.handleListIMBindings)
		r.Post("/api/sandboxes/{id}/weixin/qr-start", s.handleIMWeixinQRStart)
		r.Post("/api/sandboxes/{id}/weixin/qr-wait", s.handleIMWeixinQRWait)
	})

	return r
}

// requireWorkspaceMember checks that the requesting user is a member of the workspace.
func (s *Server) requireWorkspaceMember(w http.ResponseWriter, r *http.Request, workspaceID string) (string, bool) {
	userID := auth.UserIDFromContext(r.Context())
	role, err := s.db.GetWorkspaceMemberRole(workspaceID, userID)
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
