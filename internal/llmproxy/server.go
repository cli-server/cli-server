package llmproxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server is the LLM proxy HTTP server.
type Server struct {
	config     Config
	store      *Store
	logger     *slog.Logger
	httpClient *http.Client // for calling agentserver API
}

// NewServer creates a new LLM proxy server.
func NewServer(cfg Config, store *Store, logger *slog.Logger) *Server {
	return &Server{
		config: cfg,
		store:  store,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Routes returns the HTTP handler with all routes configured.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Health check.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Anthropic API proxy (all /v1/* paths).
	r.HandleFunc("/v1/*", s.handleAnthropicProxy)

	// Query API (requires database).
	r.Route("/api", func(r chi.Router) {
		r.Use(s.requireStore)
		r.Get("/usage", s.handleQueryUsage)
		r.Get("/traces", s.handleQueryTraces)
		r.Get("/traces/{id}", s.handleGetTrace)
	})

	return r
}

// requireStore returns 503 if the database store is not configured.
func (s *Server) requireStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil {
			http.Error(w, "database not configured", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleQueryUsage returns aggregated token usage.
func (s *Server) handleQueryUsage(w http.ResponseWriter, r *http.Request) {
	opts := parseQueryOpts(r)

	usage, err := s.store.QueryUsage(opts)
	if err != nil {
		s.logger.Error("query usage failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := map[string]interface{}{
		"usage": usage,
	}
	if !opts.Since.IsZero() {
		resp["since"] = opts.Since.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleQueryTraces returns traces with aggregated statistics.
func (s *Server) handleQueryTraces(w http.ResponseWriter, r *http.Request) {
	opts := parseQueryOpts(r)

	traces, err := s.store.QueryTraces(opts)
	if err != nil {
		s.logger.Error("query traces failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"traces": traces,
	})
}

// handleGetTrace returns a single trace with all its request records.
func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	traceID := chi.URLParam(r, "id")

	trace, requests, err := s.store.GetTraceDetail(traceID)
	if err != nil {
		s.logger.Error("get trace detail failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if trace == nil {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"trace":    trace,
		"requests": requests,
	})
}

func parseQueryOpts(r *http.Request) QueryOpts {
	opts := QueryOpts{
		WorkspaceID: r.URL.Query().Get("workspace_id"),
		SandboxID:   r.URL.Query().Get("sandbox_id"),
	}
	if since := r.URL.Query().Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			opts.Since = t
		}
	}
	if limit := r.URL.Query().Get("limit"); limit != "" {
		if n, err := strconv.Atoi(limit); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	return opts
}
