package codexexecgateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/agentserver/agentserver/internal/codexexecgateway/handlers"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server bundles the chi router with its dependencies.
// store may be nil for smoke tests that don't exercise DB paths; registry and revoked are always constructed.
type Server struct {
	config   Config
	store    *Store
	registry *ConnRegistry
	revoked  *RevokedSet
	logger   *slog.Logger
}

func NewServer(cfg Config, store *Store) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return &Server{
		config:   cfg,
		store:    store,
		registry: NewConnRegistry(),
		revoked:  NewRevokedSet(10000),
		logger:   logger,
	}, nil
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	r.Get("/codex-exec/{exe_id}", s.handleInbound)
	r.Get("/bridge/{exe_id}", s.handleBridge)

	r.Post("/api/codex-exec/register", handlers.Register(registerStoreAdapter{s.store}))

	r.Route("/api/codex-exec/workspaces/{wid}/executors", func(r chi.Router) {
		r.Post("/", handlers.PostBinding(bindingStoreAdapter{s.store}))
		r.Get("/", handlers.ListBinding(bindingStoreAdapter{s.store}))
		r.Delete("/{exe_id}", handlers.DeleteBinding(bindingStoreAdapter{s.store}))
	})

	r.Route("/api/exec-gateway", func(r chi.Router) {
		r.Use(handlers.RequireSharedSecret(s.config.InternalSharedSecret))
		r.Get("/connected", handlers.Connected(internalStoreAdapter{s.store}, s.registry))
		r.Post("/revoke-turn", handlers.RevokeTurn(s.revoked))
	})

	// More routes added in later tasks.
	return r
}

// registerStoreAdapter bridges *Store to the handlers.Store interface,
// translating between the two Executor types to avoid an import cycle.
type registerStoreAdapter struct{ s *Store }

func (a registerStoreAdapter) CreateExecutor(ctx context.Context, e handlers.Executor, hash string) error {
	return a.s.CreateExecutor(ctx, Executor{
		ExeID:        e.ExeID,
		UserID:       e.UserID,
		DisplayName:  e.DisplayName,
		Description:  e.Description,
		DefaultCwd:   e.DefaultCwd,
		RegisteredAt: e.RegisteredAt,
	}, hash)
}

// bindingStoreAdapter bridges *Store to the handlers.BindingStore interface,
// avoiding an import cycle between the handlers sub-package and its parent.
type bindingStoreAdapter struct{ s *Store }

func (a bindingStoreAdapter) BindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string, isDefault bool) error {
	return a.s.BindWorkspaceExecutor(ctx, workspaceID, exeID, isDefault)
}

func (a bindingStoreAdapter) UnbindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string) error {
	return a.s.UnbindWorkspaceExecutor(ctx, workspaceID, exeID)
}

func (a bindingStoreAdapter) ListWorkspaceExecutors(ctx context.Context, workspaceID string) ([]handlers.ConnectedExecutor, error) {
	rows, err := a.s.ListWorkspaceExecutors(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]handlers.ConnectedExecutor, len(rows))
	for i, r := range rows {
		out[i] = handlers.ConnectedExecutor{
			ExeID:       r.ExeID,
			Description: r.Description,
			DefaultCwd:  r.DefaultCwd,
			IsDefault:   r.IsDefault,
			LastSeenAt:  r.LastSeenAt,
		}
	}
	return out, nil
}

// internalStoreAdapter bridges *Store to handlers.InternalConnectedStore, converting
// []ConnectedExecutor (parent package) → []handlers.ConnectedExecutor (handlers package)
// to avoid an import cycle.
type internalStoreAdapter struct{ s *Store }

func (a internalStoreAdapter) ConnectedExecutorsForWorkspace(ctx context.Context, workspaceID string, connectedIDs []string) ([]handlers.ConnectedExecutor, error) {
	rows, err := a.s.ConnectedExecutorsForWorkspace(ctx, workspaceID, connectedIDs)
	if err != nil {
		return nil, err
	}
	out := make([]handlers.ConnectedExecutor, len(rows))
	for i, r := range rows {
		out[i] = handlers.ConnectedExecutor{
			ExeID:       r.ExeID,
			Description: r.Description,
			DefaultCwd:  r.DefaultCwd,
			IsDefault:   r.IsDefault,
			LastSeenAt:  r.LastSeenAt,
		}
	}
	return out, nil
}

// (real ConnRegistry lives in registry.go; real RevokedSet in revocation.go)
