package executorregistry

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	config  Config
	store   *Store
	tunnels *TunnelRegistry
	logger  *slog.Logger
}

func NewServer(cfg Config, store *Store) *Server {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return &Server{
		config:  cfg,
		store:   store,
		tunnels: NewTunnelRegistry(),
		logger:  logger,
	}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	r.Post("/api/executors/register", s.handleRegister)
	r.Post("/api/executors/sandbox", s.handleRegisterSandbox)
	r.Put("/api/executors/{id}/heartbeat", s.handleHeartbeat)
	r.Get("/api/executors", s.handleListExecutors)
	r.Get("/api/executors/{id}", s.handleGetExecutor)
	r.Put("/api/executors/{id}/capabilities", s.handleUpdateCapabilities)
	r.Get("/api/tunnel/{executor_id}", s.handleTunnel)
	r.Post("/api/execute", s.handleExecute)

	return r
}
