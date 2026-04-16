package ccbroker

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	config Config
	store  *Store
	sse    *SSEBroker
	dedup  *DedupRegistry
	logger *slog.Logger
}

func NewServer(cfg Config, store *Store) *Server {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return &Server{
		config: cfg,
		store:  store,
		sse:    NewSSEBroker(),
		dedup:  NewDedupRegistry(),
		logger: logger,
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

	// Session lifecycle
	r.Post("/v1/sessions", s.handleCreateSession)
	r.Post("/v1/sessions/{sessionId}/bridge", s.handleBridge)

	// Worker endpoints (JWT auth)
	r.Route("/v1/sessions/{sessionId}/worker", func(r chi.Router) {
		r.Use(s.workerAuthMiddleware)
		r.Get("/events/stream", s.handleWorkerEventStream)
		r.Post("/events", s.handleWorkerEvents)
		r.Post("/internal-events", s.handleWorkerInternalEvents)
		r.Get("/internal-events", s.handleGetInternalEvents)
		r.Put("/", s.handleWorkerState)
		r.Post("/heartbeat", s.handleWorkerHeartbeat)
	})

	return r
}

// Stub handlers -- replaced in Tasks 4-6
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request)       { w.WriteHeader(501) }
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request)              { w.WriteHeader(501) }
func (s *Server) handleWorkerEventStream(w http.ResponseWriter, r *http.Request)   { w.WriteHeader(501) }
func (s *Server) handleWorkerEvents(w http.ResponseWriter, r *http.Request)        { w.WriteHeader(501) }
func (s *Server) handleWorkerInternalEvents(w http.ResponseWriter, r *http.Request) { w.WriteHeader(501) }
func (s *Server) handleGetInternalEvents(w http.ResponseWriter, r *http.Request)   { w.WriteHeader(501) }
func (s *Server) handleWorkerState(w http.ResponseWriter, r *http.Request)         { w.WriteHeader(501) }
func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request)     { w.WriteHeader(501) }
func (s *Server) workerAuthMiddleware(next http.Handler) http.Handler              { return next }

// Helpers
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
