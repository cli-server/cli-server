package ccbroker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/runner"
	"github.com/agentserver/agentserver/internal/ccbroker/tools"
	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// TestWorkerProcessesSingleTurn asserts that, given one queued turn, the
// worker calls execute exactly once, then sleeps until idle timeout.
func TestWorkerProcessesSingleTurn(t *testing.T) {
	store := newFakeStore()
	sid := "sess_x"
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: "ws_y"}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_1", SessionID: sid, WorkspaceID: "ws_y", UserEventID: "evt_a", UserMessage: "hi",
	})

	var executed atomic.Int32
	var idleExited atomic.Int32
	w := &sessionWorker{
		sessionID: sid,
		wake:      make(chan struct{}, 1),
		quit:      make(chan struct{}),
		idleAfter: 50 * time.Millisecond,
		deps: workerDeps{
			store:  store,
			logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		},
		executeFn: func(_ context.Context, turn *AgentTurn) {
			executed.Add(1)
			// Stub stands in for the real execute body (Task 5),
			// which is responsible for advancing the turn out of the
			// queued/running set so PickNextPending stops returning it.
			_ = store.MarkTurnDone(context.Background(), turn.ID)
		},
		onIdleExit: func(_ string) { idleExited.Add(1) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("worker did not idle-exit in time")
	}
	if executed.Load() != 1 {
		t.Fatalf("expected execute once, got %d", executed.Load())
	}
	if idleExited.Load() != 1 {
		t.Fatalf("expected onIdleExit once, got %d", idleExited.Load())
	}
}

func TestWorkerWakesFromIdle(t *testing.T) {
	store := newFakeStore()
	sid := "sess_w"

	var executed atomic.Int32
	w := &sessionWorker{
		sessionID: sid,
		wake:      make(chan struct{}, 1),
		quit:      make(chan struct{}),
		idleAfter: 5 * time.Second, // long; we'll wake via Notify
		deps: workerDeps{
			store:  store,
			logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		},
		executeFn: func(_ context.Context, turn *AgentTurn) {
			executed.Add(1)
			_ = store.MarkTurnDone(context.Background(), turn.ID)
		},
		onIdleExit: func(_ string) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.run(ctx)

	// Worker should be sleeping (no turns enqueued yet).
	time.Sleep(50 * time.Millisecond)
	if executed.Load() != 0 {
		t.Fatalf("worker should not have executed yet")
	}

	// Enqueue, wake, expect execute.
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_w", SessionID: sid, WorkspaceID: "ws", UserEventID: "evt", UserMessage: "hi",
	})
	w.wake <- struct{}{}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if executed.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if executed.Load() != 1 {
		t.Fatalf("worker did not execute after wake (got %d)", executed.Load())
	}
	close(w.quit)
}

func TestWorkerExitsOnQuit(t *testing.T) {
	w := &sessionWorker{
		sessionID: "sess_q",
		wake:      make(chan struct{}, 1),
		quit:      make(chan struct{}),
		idleAfter: 10 * time.Second,
		deps: workerDeps{
			store:  newFakeStore(),
			logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		},
		executeFn:  func(_ context.Context, _ *AgentTurn) {},
		onIdleExit: func(_ string) {},
	}
	done := make(chan struct{})
	go func() { w.run(context.Background()); close(done) }()
	close(w.quit)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("worker did not exit on quit")
	}
}

func TestExecuteHappyPath(t *testing.T) {
	sid, wid, tid := "sess_e", "ws_e", "trn_e"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt_u", UserMessage: "hello",
	})

	sse := NewSSEBroker()
	sub := sse.Subscribe(sid)
	defer sse.Unsubscribe(sid, sub)

	teardownCalled := atomic.Int32{}
	deps := workerDeps{
		store:        store,
		s3:           nil,
		sse:          sse,
		activeTurns:  newActiveTurnRegistry(),
		compactQueue: newCompactQueue(),
		gate:         tools.NewGate(func(string, tools.Event) {}),
		logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient:   http.DefaultClient,
		wstoken: func(_ context.Context, _ string) (string, error) {
			return "tok", nil
		},
		workspaceSetup: func(_ context.Context, w, s string, _ *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{WorkspaceID: w, SessionID: s, TempDir: "/tmp/x"}, nil
		},
		workspaceTeardown: func(_ context.Context, _ *workspace.Workspace, _ *workspace.S3Store) error {
			teardownCalled.Add(1)
			return nil
		},
		runnerRun: func(_ context.Context, _ *workspace.Workspace, _, _ string, _ runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			ch := make(chan agentsdk.SDKMessage, 2)
			ch <- agentsdk.SDKMessage{Type: "assistant", Raw: json.RawMessage(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)}
			ch <- agentsdk.SDKMessage{Type: "result", Subtype: "success", Raw: json.RawMessage(`{"type":"result","subtype":"success","is_error":false}`)}
			close(ch)
			return ch, nil
		},
		callTurnFinished: func(_, _ string) {},
	}
	w := newSessionWorker(sid, deps, func(string) {})

	turn, _ := store.PickNextPending(context.Background(), sid)
	if turn == nil {
		t.Fatalf("expected pending turn")
	}
	w.execute(context.Background(), turn)

	if teardownCalled.Load() != 1 {
		t.Fatalf("teardown not called")
	}
	got, _ := store.GetTurn(context.Background(), tid)
	if got == nil || got.State != "done" {
		t.Fatalf("expected done, got %+v", got)
	}

	// Drain published events; expect at least one with our TurnID.
	gotTurnID := false
loop:
	for {
		select {
		case ev := <-sub.Ch:
			if ev.TurnID == tid {
				gotTurnID = true
			}
		default:
			break loop
		}
	}
	if !gotTurnID {
		t.Fatalf("no event tagged with TurnID")
	}
}

func TestExecuteCancelMidStream(t *testing.T) {
	sid, wid, tid := "sess_c", "ws_c", "trn_c"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt", UserMessage: "x",
	})

	deps := workerDeps{
		store: store, sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		gate:       tools.NewGate(func(string, tools.Event) {}),
		logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient: http.DefaultClient,
		wstoken:    func(context.Context, string) (string, error) { return "t", nil },
		workspaceSetup: func(context.Context, string, string, *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{}, nil
		},
		workspaceTeardown: func(context.Context, *workspace.Workspace, *workspace.S3Store) error { return nil },
		runnerRun: func(ctx context.Context, _ *workspace.Workspace, _, _ string, _ runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			ch := make(chan agentsdk.SDKMessage)
			go func() {
				<-ctx.Done() // block until cancelled, then close
				close(ch)
			}()
			return ch, nil
		},
		callTurnFinished: func(string, string) {},
	}
	w := newSessionWorker(sid, deps, func(string) {})
	turn, _ := store.PickNextPending(context.Background(), sid)

	// Cancel via activeTurns from another goroutine after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		deps.activeTurns.Cancel(sid, tid)
	}()

	w.execute(context.Background(), turn)
	got, _ := store.GetTurn(context.Background(), tid)
	if got == nil || got.State != "cancelled" {
		t.Fatalf("expected cancelled, got %+v", got)
	}
}

func TestExecuteThreadsMetadataIntoRunCfg(t *testing.T) {
	sid, wid, tid := "sess_md", "ws_md", "trn_md"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	metaBytes, _ := json.Marshal(TurnMetadata{
		ChannelType:         "tui",
		CreatorUserID:       "user_42",
		PermissionMode:      "ask",
		Model:               "claude-opus-4-7",
		PreferredExecutorID: "exec_x",
	})
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt", UserMessage: "x",
		Metadata: metaBytes,
	})

	var capturedCfg runner.Config
	deps := workerDeps{
		store: store, sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		gate:       tools.NewGate(func(string, tools.Event) {}),
		logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient: http.DefaultClient,
		wstoken:    func(context.Context, string) (string, error) { return "t", nil },
		workspaceSetup: func(context.Context, string, string, *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{}, nil
		},
		workspaceTeardown: func(context.Context, *workspace.Workspace, *workspace.S3Store) error { return nil },
		runnerRun: func(_ context.Context, _ *workspace.Workspace, _, _ string, cfg runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			capturedCfg = cfg
			ch := make(chan agentsdk.SDKMessage)
			close(ch)
			return ch, nil
		},
		callTurnFinished: func(string, string) {},
	}
	w := newSessionWorker(sid, deps, func(string) {})
	turn, _ := store.PickNextPending(context.Background(), sid)
	w.execute(context.Background(), turn)

	if capturedCfg.ChannelType != "tui" {
		t.Fatalf("ChannelType: want tui, got %q", capturedCfg.ChannelType)
	}
	if capturedCfg.CreatorUserID != "user_42" {
		t.Fatalf("CreatorUserID: want user_42, got %q", capturedCfg.CreatorUserID)
	}
	if capturedCfg.PermissionMode != "ask" {
		t.Fatalf("PermissionMode: want ask, got %q", capturedCfg.PermissionMode)
	}
	if capturedCfg.Model != "claude-opus-4-7" {
		t.Fatalf("Model: want claude-opus-4-7, got %q", capturedCfg.Model)
	}
	if capturedCfg.PreferredExecutorID != "exec_x" {
		t.Fatalf("PreferredExecutorID: want exec_x, got %q", capturedCfg.PreferredExecutorID)
	}
}

func TestExecuteAppliesIMDefaults(t *testing.T) {
	sid, wid, tid := "sess_def", "ws_def", "trn_def"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt", UserMessage: "x",
	})

	var capturedCfg runner.Config
	deps := workerDeps{
		store: store, sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		gate:       tools.NewGate(func(string, tools.Event) {}),
		logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient: http.DefaultClient,
		wstoken:    func(context.Context, string) (string, error) { return "t", nil },
		workspaceSetup: func(context.Context, string, string, *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{}, nil
		},
		workspaceTeardown: func(context.Context, *workspace.Workspace, *workspace.S3Store) error { return nil },
		runnerRun: func(_ context.Context, _ *workspace.Workspace, _, _ string, cfg runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			capturedCfg = cfg
			ch := make(chan agentsdk.SDKMessage)
			close(ch)
			return ch, nil
		},
		callTurnFinished: func(string, string) {},
	}
	w := newSessionWorker(sid, deps, func(string) {})
	turn, _ := store.PickNextPending(context.Background(), sid)
	w.execute(context.Background(), turn)

	if capturedCfg.ChannelType != "im" {
		t.Fatalf("ChannelType default: want im, got %q", capturedCfg.ChannelType)
	}
	if capturedCfg.PermissionMode != "bypass" {
		t.Fatalf("PermissionMode default: want bypass, got %q", capturedCfg.PermissionMode)
	}
}

func TestExecuteHonorsCompactQueueAndCallsTurnFinished(t *testing.T) {
	sid, wid, tid := "sess_cq", "ws_cq", "trn_cq"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt", UserMessage: "x",
	})

	var (
		capturedCfg     runner.Config
		finishedCount   atomic.Int32
		capturedTid     string
		capturedTidOk   bool
	)
	activeTurns := newActiveTurnRegistry()
	compactQ := newCompactQueue()
	compactQ.Set(sid)

	deps := workerDeps{
		store: store, sse: NewSSEBroker(),
		activeTurns: activeTurns, compactQueue: compactQ,
		gate:       tools.NewGate(func(string, tools.Event) {}),
		logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient: http.DefaultClient,
		wstoken:    func(context.Context, string) (string, error) { return "t", nil },
		workspaceSetup: func(context.Context, string, string, *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{}, nil
		},
		workspaceTeardown: func(context.Context, *workspace.Workspace, *workspace.S3Store) error { return nil },
		runnerRun: func(_ context.Context, _ *workspace.Workspace, _, _ string, cfg runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			capturedCfg = cfg
			// Verify activeTurns.Set was called BEFORE the runner runs.
			capturedTid, capturedTidOk = activeTurns.Get(sid)
			ch := make(chan agentsdk.SDKMessage)
			close(ch)
			return ch, nil
		},
		callTurnFinished: func(_, _ string) { finishedCount.Add(1) },
	}
	w := newSessionWorker(sid, deps, func(string) {})
	turn, _ := store.PickNextPending(context.Background(), sid)
	w.execute(context.Background(), turn)

	if capturedCfg.TurnKind != "compaction" {
		t.Fatalf("TurnKind: want compaction, got %q", capturedCfg.TurnKind)
	}
	if compactQ.IsSet(sid) {
		t.Fatalf("compactQueue should be consumed by Take")
	}
	if got := finishedCount.Load(); got != 1 {
		t.Fatalf("callTurnFinished count: want 1, got %d", got)
	}
	if gotTid, ok := activeTurns.Get(sid); ok || gotTid != "" {
		t.Fatalf("activeTurns should be cleared, got (%q, %v)", gotTid, ok)
	}
	if !capturedTidOk || capturedTid != tid {
		t.Fatalf("activeTurns.Set should run before runner; got (%q, %v)", capturedTid, capturedTidOk)
	}
}

func TestExecuteRunnerError(t *testing.T) {
	sid, wid, tid := "sess_re", "ws_re", "trn_re"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt", UserMessage: "x",
	})

	deps := workerDeps{
		store: store, sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		gate:       tools.NewGate(func(string, tools.Event) {}),
		logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient: http.DefaultClient,
		wstoken:    func(context.Context, string) (string, error) { return "t", nil },
		workspaceSetup: func(context.Context, string, string, *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{}, nil
		},
		workspaceTeardown: func(context.Context, *workspace.Workspace, *workspace.S3Store) error { return nil },
		runnerRun: func(context.Context, *workspace.Workspace, string, string, runner.Config, *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			return nil, errors.New("boom")
		},
		callTurnFinished: func(string, string) {},
	}
	w := newSessionWorker(sid, deps, func(string) {})
	turn, _ := store.PickNextPending(context.Background(), sid)
	w.execute(context.Background(), turn)
	got, _ := store.GetTurn(context.Background(), tid)
	if got == nil || got.State != "failed" {
		t.Fatalf("expected failed, got %+v", got)
	}
}
