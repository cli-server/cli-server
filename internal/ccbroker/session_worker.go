package ccbroker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
	"github.com/google/uuid"

	"github.com/agentserver/agentserver/internal/ccbroker/runner"
	"github.com/agentserver/agentserver/internal/ccbroker/tools"
	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

const defaultWorkerIdleTimeout = 5 * time.Minute

// workerDeps bundles everything a sessionWorker needs to run a turn.
// Function-typed callbacks (workspaceSetup, runnerRun, callTurnFinished)
// match the existing package seams in handler_turns.go, so tests can stub
// without holding a real Server.
type workerDeps struct {
	store             storer
	s3                *workspace.S3Store
	wstoken           func(ctx context.Context, workspaceID string) (string, error)
	sse               *SSEBroker
	activeTurns       *activeTurnRegistry
	compactQueue      *compactQueue
	gate              *tools.Gate
	logger            *slog.Logger
	config            Config
	httpClient        *http.Client
	workspaceSetup    func(ctx context.Context, wid, sid string, s3 *workspace.S3Store) (*workspace.Workspace, error)
	workspaceTeardown func(ctx context.Context, ws *workspace.Workspace, s3 *workspace.S3Store) error
	runnerRun         func(ctx context.Context, ws *workspace.Workspace, sid, msg string, cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error)
	callTurnFinished  func(sid, tid string)
}

// sessionWorker drains the queue for one session_id. Each Notify wake-up
// causes it to PickNextPending in a loop until empty, then sleep until
// idleAfter elapses, at which point it removes itself from the registry.
type sessionWorker struct {
	sessionID  string
	wake       chan struct{} // buffer=1
	quit       chan struct{}
	idleAfter  time.Duration
	deps       workerDeps
	onIdleExit func(sessionID string)

	// executeFn is the heavy path; tests inject a stub. Production wiring
	// (Task 5) defaults this to (*sessionWorker).execute.
	executeFn func(ctx context.Context, t *AgentTurn)
}

func newSessionWorker(sessionID string, deps workerDeps, onIdleExit func(string)) *sessionWorker {
	w := &sessionWorker{
		sessionID:  sessionID,
		wake:       make(chan struct{}, 1),
		quit:       make(chan struct{}),
		idleAfter:  defaultWorkerIdleTimeout,
		deps:       deps,
		onIdleExit: onIdleExit,
	}
	w.executeFn = w.execute
	return w
}

func (w *sessionWorker) run(ctx context.Context) {
	idle := time.NewTimer(w.idleAfter)
	defer idle.Stop()
	for {
		turn, err := w.deps.store.PickNextPending(ctx, w.sessionID)
		if err != nil {
			w.deps.logger.Error("worker pick next failed",
				"session_id", w.sessionID, "error", err)
			select {
			case <-time.After(time.Second):
			case <-w.quit:
				return
			case <-ctx.Done():
				return
			}
			continue
		}
		if turn != nil {
			w.executeFn(ctx, turn)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(w.idleAfter)
			continue
		}
		select {
		case <-w.wake:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(w.idleAfter)
		case <-idle.C:
			if w.onIdleExit != nil {
				w.onIdleExit(w.sessionID)
			}
			return
		case <-w.quit:
			return
		case <-ctx.Done():
			return
		}
	}
}

// execute is the production heavy path: it owns one turn's full lifecycle
// (workspace setup, runner.Run, SSE pump, terminal-state mark, teardown).
// Mirrors the body of handler_turns.handleProcessTurn pre-Task 8.
func (w *sessionWorker) execute(ctx context.Context, turn *AgentTurn) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.deps.activeTurns.Set(turn.SessionID, turn.ID, cancel)
	defer w.deps.activeTurns.Clear(turn.SessionID, turn.ID)
	defer w.deps.callTurnFinished(turn.SessionID, turn.ID)

	if err := w.deps.store.MarkTurnRunning(ctx, turn.ID); err != nil {
		w.deps.logger.Error("worker mark running failed",
			"session_id", turn.SessionID, "turn_id", turn.ID, "error", err)
		return
	}

	wsTok, err := w.deps.wstoken(ctx, turn.WorkspaceID)
	if err != nil {
		w.failTurn(ctx, turn, "workspace token: "+err.Error())
		return
	}

	ws, err := w.deps.workspaceSetup(ctx, turn.WorkspaceID, turn.SessionID, w.deps.s3)
	if err != nil {
		w.failTurn(ctx, turn, "workspace setup: "+err.Error())
		return
	}
	defer func() {
		_ = w.deps.workspaceTeardown(context.Background(), ws, w.deps.s3)
	}()

	// Honor compaction request queued via /compact since the previous turn.
	turnKind := ""
	if w.deps.compactQueue.Take(turn.SessionID) {
		turnKind = "compaction"
	}

	// Decode metadata for per-turn settings.
	var meta TurnMetadata
	if len(turn.Metadata) > 0 {
		_ = json.Unmarshal(turn.Metadata, &meta)
	}
	channelType := defaultStr(meta.ChannelType, "im")
	permMode := defaultStr(meta.PermissionMode, "bypass")

	tctx := &tools.Context{
		SessionID:              turn.SessionID,
		WorkspaceID:            turn.WorkspaceID,
		IMChannelID:            turn.IMChannelID.String,
		IMUserID:               turn.IMUserID.String,
		ExecutorRegistryURL:    w.deps.config.ExecutorRegistryURL,
		AgentserverURL:         w.deps.config.AgentserverURL,
		IMBridgeURL:            w.deps.config.IMBridgeURL,
		InternalAPISecret:      w.deps.config.IMBridgeSecret,
		Workspace:              ws,
		HTTP:                   w.deps.httpClient,
		ChannelType:            channelType,
		CreatorUserID:          meta.CreatorUserID,
		PermissionMode:         permMode,
		PreferredExecutorID:    meta.PreferredExecutorID,
		Gate:                   w.deps.gate,
		AgentserverInternalURL: w.deps.config.AgentserverInternalURL,
		CurrentTurnID:          turn.ID,
	}
	mcp := tools.BuildMcpServer(tctx)

	runCfg := runner.Config{
		SystemPrompt:             "",
		MaxTurns:                 0,
		AnthropicAuthToken:       wsTok,
		AnthropicBaseURL:         w.deps.config.LLMProxyURL,
		DisableFileCheckpointing: true,
		AutoCompactWindow:        165000,
		SessionID:                turn.SessionID,
		TurnID:                   turn.ID,
		ChannelType:              channelType,
		CreatorUserID:            meta.CreatorUserID,
		PermissionMode:           permMode,
		Model:                    meta.Model,
		PreferredExecutorID:      meta.PreferredExecutorID,
		TurnKind:                 turnKind,
	}

	msgCh, err := w.deps.runnerRun(turnCtx, ws, turn.SessionID, turn.UserMessage, runCfg, mcp)
	if err != nil {
		w.failTurn(ctx, turn, "runner.Run: "+err.Error())
		return
	}

	epoch, err := w.deps.store.GetSessionEpoch(ctx, turn.SessionID)
	if err != nil {
		w.deps.logger.Warn("get epoch failed", "session_id", turn.SessionID, "error", err)
	}

	for sdkMsg := range msgCh {
		evt, convErr := runner.ToEventPayload(sdkMsg)
		if convErr != nil {
			w.deps.logger.Warn("ToEventPayload failed",
				"session_id", turn.SessionID, "error", convErr)
			continue
		}
		eventID := uuid.NewString()
		var seqNum int64
		if !evt.Ephemeral {
			inserted, insertErr := w.deps.store.InsertEventsWithTurn(
				context.Background(), turn.SessionID, epoch, turn.ID,
				[]EventInput{{EventID: eventID, Payload: evt.Payload, Ephemeral: false}},
			)
			if insertErr != nil {
				w.deps.logger.Warn("InsertEventsWithTurn failed",
					"session_id", turn.SessionID, "error", insertErr)
			} else if len(inserted) > 0 {
				seqNum = inserted[0].SeqNum
			}
		}
		w.deps.sse.Publish(turn.SessionID, &StreamClientEvent{
			EventID:     eventID,
			SequenceNum: seqNum,
			EventType:   "client_event",
			Source:      "worker",
			TurnID:      turn.ID,
			Payload:     evt.Payload,
			CreatedAt:   time.Now().Format(time.RFC3339Nano),
		})
	}

	if turnCtx.Err() != nil {
		_ = w.deps.store.MarkTurnCancelled(context.Background(), turn.ID)
		w.publishTerminal(turn, "turn_cancelled")
		return
	}
	_ = w.deps.store.MarkTurnDone(context.Background(), turn.ID)
	w.publishTerminal(turn, "turn_done")
}

func (w *sessionWorker) failTurn(_ context.Context, turn *AgentTurn, msg string) {
	w.deps.logger.Error("turn failed",
		"session_id", turn.SessionID, "turn_id", turn.ID, "error", msg)
	_ = w.deps.store.MarkTurnFailed(context.Background(), turn.ID, msg)
	payload, _ := json.Marshal(map[string]string{"turn_id": turn.ID, "error": msg})
	w.deps.sse.Publish(turn.SessionID, &StreamClientEvent{
		EventID:   "evt_" + uuid.NewString(),
		EventType: "turn_failed",
		Source:    "worker",
		TurnID:    turn.ID,
		Payload:   payload,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	})
}

func (w *sessionWorker) publishTerminal(turn *AgentTurn, eventType string) {
	payload, _ := json.Marshal(map[string]string{"turn_id": turn.ID})
	w.deps.sse.Publish(turn.SessionID, &StreamClientEvent{
		EventID:   "evt_" + uuid.NewString(),
		EventType: eventType,
		Source:    "worker",
		TurnID:    turn.ID,
		Payload:   payload,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	})
}
