package ccbroker

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"
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
