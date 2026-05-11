package codexexecgateway

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server bundles the chi router with its dependencies.
// store, registry, revoked may be nil during smoke tests.
type Server struct {
	config   Config
	store    *Store
	registry *ConnRegistry
	revoked  *RevokedSet
	logger   *slog.Logger
}

func NewServer(cfg Config, store *Store) *Server {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return &Server{
		config:   cfg,
		store:    store,
		registry: NewConnRegistry(),
		revoked:  NewRevokedSet(10000),
		logger:   logger,
	}
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

	// More routes added in later tasks.
	return r
}

// (real ConnRegistry lives in registry.go; real RevokedSet in revocation.go)
