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

	// Internal API (requires database, network-isolated — only agentserver can reach these).
	r.Route("/internal", func(r chi.Router) {
		r.Use(s.requireStore)
		r.Get("/usage", s.handleQueryUsage)
		r.Get("/traces", s.handleQueryTraces)
		r.Get("/traces/{id}", s.handleGetTrace)
		r.Get("/quotas/{workspace_id}", s.handleGetWorkspaceQuota)
		r.Put("/quotas/{workspace_id}", s.handleSetWorkspaceQuota)
		r.Delete("/quotas/{workspace_id}", s.handleDeleteWorkspaceQuota)
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

	traces, total, err := s.store.QueryTraces(opts)
	if err != nil {
		s.logger.Error("query traces failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"traces": traces,
		"total":  total,
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

// handleGetWorkspaceQuota returns the quota override and config default for a workspace.
func (s *Server) handleGetWorkspaceQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "workspace_id")

	wq, err := s.store.GetWorkspaceQuota(workspaceID)
	if err != nil {
		s.logger.Error("get workspace quota failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	todayCount, err := s.store.CountTodayRequests(workspaceID)
	if err != nil {
		s.logger.Error("count today requests failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"workspace_quota":     wq,
		"default_max_rpd":     s.config.DefaultMaxRPD,
		"today_request_count": todayCount,
	})
}

// handleSetWorkspaceQuota sets the quota override for a workspace.
func (s *Server) handleSetWorkspaceQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "workspace_id")

	var req struct {
		MaxRPD *int `json:"max_rpd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.MaxRPD != nil && *req.MaxRPD < 0 {
		http.Error(w, "max_rpd must be >= 0", http.StatusBadRequest)
		return
	}

	if err := s.store.SetWorkspaceQuota(workspaceID, req.MaxRPD); err != nil {
		s.logger.Error("set workspace quota failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteWorkspaceQuota removes the quota override for a workspace.
func (s *Server) handleDeleteWorkspaceQuota(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "workspace_id")

	if err := s.store.DeleteWorkspaceQuota(workspaceID); err != nil {
		s.logger.Error("delete workspace quota failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
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
	if offset := r.URL.Query().Get("offset"); offset != "" {
		if n, err := strconv.Atoi(offset); err == nil && n >= 0 {
			opts.Offset = n
		}
	}
	return opts
}
