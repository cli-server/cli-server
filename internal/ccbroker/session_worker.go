package ccbroker

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

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

// execute is the production heavy path; populated in Task 5.
func (w *sessionWorker) execute(ctx context.Context, t *AgentTurn) {
	// Implemented in Task 5.
}
