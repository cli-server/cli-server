package ccbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	config   Config
	store    *Store
	sse      *SSEBroker
	dedup    *DedupRegistry
	turnLock *TurnLock
	logger   *slog.Logger
}

func NewServer(cfg Config, store *Store) *Server {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return &Server{
		config:   cfg,
		store:    store,
		sse:      NewSSEBroker(),
		dedup:    NewDedupRegistry(),
		turnLock: NewTurnLock(),
		logger:   logger,
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

	// External API
	r.Post("/api/turns", s.handleProcessTurn)

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

// CreateMCPServer creates a per-worker MCP server and returns (server, port, closer, error).
// The caller should call closer() to stop the MCP server when done.
func (s *Server) CreateMCPServer(sessionID, workspaceID, workspaceDir, imChannelID, imUserID string) (*MCPServer, int, func(), error) {
	router := NewToolRouter(ToolRouterConfig{
		ExecutorRegistryURL: s.config.ExecutorRegistryURL,
		AgentserverURL:      s.config.AgentserverURL,
		IMBridgeURL:         s.config.IMBridgeURL,
		IMBridgeSecret:      s.config.IMBridgeSecret,
		WorkspaceDir:        workspaceDir,
		SessionID:           sessionID,
		WorkspaceID:         workspaceID,
		IMChannelID:         imChannelID,
		IMUserID:            imUserID,
	}, s.logger)

	mcpSrv := NewMCPServer(router, s.logger)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	httpSrv := &http.Server{Handler: mcpSrv}
	go httpSrv.Serve(listener) //nolint:errcheck

	closer := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpSrv.Shutdown(ctx)
	}
	return mcpSrv, port, closer, nil
}

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
