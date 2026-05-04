package ccbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/runner"
	"github.com/agentserver/agentserver/internal/ccbroker/tools"
	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
	"github.com/agentserver/agentserver/internal/ccbroker/wstoken"
)

// storer abstracts the database operations needed by the Server. The concrete
// implementation is *Store (backed by Postgres); tests inject a fakeStore.
type storer interface {
	GetSession(ctx context.Context, id string) (*Session, error)
	CreateSession(ctx context.Context, id, workspaceID, title, source string, externalID *string) error
	GetSessionEpoch(ctx context.Context, sessionID string) (int, error)
	InsertEvents(ctx context.Context, sessionID string, epoch int, events []EventInput) ([]InsertedEvent, error)
	InsertEventsWithTurn(ctx context.Context, sessionID string, epoch int, turnID string, events []EventInput) ([]InsertedEvent, error)

	// Turn queue ops
	EnqueueTurn(ctx context.Context, t AgentTurn) error
	PickNextPending(ctx context.Context, sessionID string) (*AgentTurn, error)
	MarkTurnRunning(ctx context.Context, turnID string) error
	MarkTurnDone(ctx context.Context, turnID string) error
	MarkTurnCancelled(ctx context.Context, turnID string) error
	MarkTurnFailed(ctx context.Context, turnID, errMsg string) error
	GetTurn(ctx context.Context, turnID string) (*AgentTurn, error)
	ListSessionsWithPending(ctx context.Context) ([]string, error)
	ListSessionTurns(ctx context.Context, sessionID string, limit int) ([]AgentTurn, error)
	ResetRunningToQueued(ctx context.Context) (int, error)
	CountPending(ctx context.Context, sessionID string) (int, error)
	GetTurnEvents(ctx context.Context, turnID string, sinceSeqNum int64) ([]TurnEvent, error)
}

type Server struct {
	config Config
	store  storer
	s3     *workspace.S3Store
	// wstoken returns the workspace's proxy token (cached or freshly fetched
	// from agentserver). Function-typed so tests can stub it without
	// standing up an HTTP fake.
	wstoken func(ctx context.Context, workspaceID string) (string, error)
	sse     *SSEBroker
	logger  *slog.Logger
	gate    *tools.Gate // permission gate, initialized in NewServer

	// Task 12: TUI control endpoints
	activeTurns  *activeTurnRegistry
	compactQueue *compactQueue

	workerRegistry *workerRegistry
}

func NewServer(cfg Config, store *Store) (*Server, error) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	s3, err := workspace.NewS3Store(workspace.S3Config{
		Endpoint:        cfg.S3Endpoint,
		Region:          cfg.S3Region,
		Bucket:          cfg.S3Bucket,
		AccessKeyID:     cfg.S3AccessKeyID,
		SecretAccessKey: cfg.S3SecretAccessKey,
		PathStyle:       cfg.S3PathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("init s3 store: %w", err)
	}
	wstokenClient := wstoken.New(cfg.AgentserverURL, cfg.IMBridgeSecret)
	s := &Server{
		config:       cfg,
		store:        store,
		s3:           s3,
		wstoken:      wstokenClient.GetOrCreate,
		sse:          NewSSEBroker(),
		logger:       logger,
		activeTurns:  newActiveTurnRegistry(),
		compactQueue: newCompactQueue(),
	}
	s.gate = tools.NewGate(func(sid string, e tools.Event) {
		payload, err := json.Marshal(e)
		if err != nil {
			s.logger.Warn("permission event marshal failed",
				"session_id", sid, "type", e.Type, "err", err)
			return
		}
		s.sse.Publish(sid, &StreamClientEvent{
			EventID:   "evt_" + uuid.NewString(),
			EventType: e.Type,
			Source:    "gate",
			Payload:   payload,
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		})
	})

	deps := workerDeps{
		store:             store,
		s3:                s3,
		wstoken:           s.wstoken,
		sse:               s.sse,
		activeTurns:       s.activeTurns,
		compactQueue:      s.compactQueue,
		gate:              s.gate,
		logger:            logger,
		config:            cfg,
		httpClient:        http.DefaultClient,
		workspaceSetup:    workspace.Setup,
		workspaceTeardown: workspace.Teardown,
		runnerRun: func(ctx context.Context, ws *workspace.Workspace, sid, msg string, cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			return runner.Run(ctx, ws, sid, msg, cfg, mcp)
		},
		callTurnFinished: s.callTurnFinished,
	}
	s.workerRegistry = newWorkerRegistry(deps)

	return s, nil
}

// Start runs one-time startup work (recovery) before HTTP serving begins.
func (s *Server) Start(ctx context.Context) error {
	return s.recoverPendingTurns(ctx)
}

// Shutdown stops worker goroutines and waits up to ctx deadline for them
// to drain. Best-effort: a stuck worker is abandoned and its turn will be
// reset on next process start.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.workerRegistry.Shutdown(ctx)
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	r.Post("/api/turns", s.handleProcessTurn)
	r.Post("/api/v2/turns", s.handleProcessTurnV2)
	r.Post("/v1/sessions", s.handleCreateSession)

	// Task 12: TUI control endpoints
	r.Post("/api/sessions/{sid}/turns/{tid}/cancel", s.handleCancelTurn)
	r.Post("/api/sessions/{sid}/permissions/{pid}/decide", s.handleDecidePermission)
	r.Post("/api/sessions/{sid}/compact", s.handleCompactNow)
	r.Get("/api/sessions/{sid}/turns/active", s.handleGetActiveTurn)

	r.Get("/api/turns/{tid}/events", s.handleTurnEvents)
	r.Get("/api/sessions/{sid}/turns", s.handleListSessionTurns)

	return r
}

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
