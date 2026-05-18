package codexexecgateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/agentserver/agentserver/internal/codexexecgateway/handlers"
	"github.com/agentserver/agentserver/internal/codexexecgateway/relay"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server bundles the chi router with its dependencies.
// Server wires the routes for codex-exec-gateway. Production must
// always be constructed with a real *Store; tests that exercise only
// auth-rejection paths may use newServerNoStoreForTesting.
type Server struct {
	config        Config
	store         *Store
	registry      *ConnRegistry
	revoked       *RevokedSet
	relayRegistry *relay.Registry // nil if PublicHTTPSBaseURL unset (dev/disabled)
	logger        *slog.Logger
}

// NewServer is the production constructor. Refuses a nil store so a
// misconfigured deploy can't silently bypass the /bridge ownership
// check (which falls back to "skip + warn" when store is nil for the
// sake of test wiring).
func NewServer(cfg Config, store *Store) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if store == nil {
		return nil, fmt.Errorf("codexexecgateway: store is required")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	var relayReg *relay.Registry
	if cfg.PublicHTTPSBaseURL != "" {
		relayReg = relay.NewRegistry(cfg.RelayMaxPerWorkspace, cfg.RelayDefaultTTL, logger)
	}
	return &Server{
		config:        cfg,
		store:         store,
		registry:      NewConnRegistry(),
		revoked:       NewRevokedSet(10000),
		relayRegistry: relayReg,
		logger:        logger,
	}, nil
}

// newServerNoStoreForTesting constructs a Server with a nil store. ONLY
// for tests in this package that exercise routes which fail before
// reaching the store. The /bridge handler logs an explicit warning and
// skips the workspace-ownership check when store is nil.
func newServerNoStoreForTesting(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	var relayReg *relay.Registry
	if cfg.PublicHTTPSBaseURL != "" {
		relayReg = relay.NewRegistry(cfg.RelayMaxPerWorkspace, cfg.RelayDefaultTTL, logger)
	}
	return &Server{
		config:        cfg,
		store:         nil,
		registry:      NewConnRegistry(),
		revoked:       NewRevokedSet(10000),
		relayRegistry: relayReg,
		logger:        logger,
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

	// HTTP relay public endpoints — ticket Bearer is auth; no other
	// middleware. Registered even when relayRegistry is nil so the
	// handlers can return a clear 404 to misconfigured callers.
	r.Put("/relay/{ticket}", s.handleRelayPut)
	r.Get("/relay/{ticket}", s.handleRelayGet)

	// Upstream codex `exec-server --remote` compat: clients POST here
	// with bearer auth, get back the ws URL above.
	r.Post("/cloud/executor/{exe_id}/register", handlers.CloudRegister(s.store, s.config.PublicWSBaseURL))

	// *Store satisfies handlers.Store, handlers.BindingStore, and
	// handlers.InternalConnectedStore directly — no adapter needed because
	// all three interfaces now use execmodel types, which *Store also uses
	// (via the type aliases in models.go).
	r.Route("/api/codex-exec", func(r chi.Router) {
		r.Use(handlers.RequireAgentserverSecret(s.config.AgentserverInternalSecret))
		r.Post("/register", handlers.Register(s.store))
		// Used by agentserver to clean up an orphaned executor after a
		// register-then-bind failure (v0.54.2). CASCADE on
		// workspace_executors handles any leftover binding rows.
		r.Delete("/executors/{exe_id}", handlers.DeleteExecutor(s.store))
		r.Route("/workspaces/{wid}/executors", func(r chi.Router) {
			r.Post("/", handlers.PostBinding(s.store))
			r.Get("/", handlers.ListBinding(s.store))
			r.Delete("/{exe_id}", handlers.DeleteBinding(s.store))
		})
	})

	r.Route("/api/exec-gateway", func(r chi.Router) {
		r.Use(handlers.RequireSharedSecret(s.config.InternalSharedSecret))
		r.Get("/connected", handlers.Connected(s.store, s.registry))
		r.Post("/revoke-turn", handlers.RevokeTurn(s.revoked))
		r.Post("/relay/create", s.handleRelayCreate)
	})

	// More routes added in later tasks.
	return r
}

// (real ConnRegistry lives in registry.go; real RevokedSet in revocation.go)
