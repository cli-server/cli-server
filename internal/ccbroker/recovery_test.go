package ccbroker

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

func TestRecoveryResetsRunningAndNotifies(t *testing.T) {
	store := newFakeStore()
	// Simulate a running turn left over from a crashed pod.
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_r1", SessionID: "sess_r1", WorkspaceID: "ws", UserEventID: "e", UserMessage: "x",
	})
	_ = store.MarkTurnRunning(context.Background(), "trn_r1")
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_r2", SessionID: "sess_r2", WorkspaceID: "ws", UserEventID: "e2", UserMessage: "y",
	})

	registry := newWorkerRegistry(workerDeps{
		store:  store,
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	registry.executeOverride = func(_ context.Context, _ *AgentTurn) {}
	defer registry.Shutdown(context.Background())

	s := &Server{
		store:          store,
		workerRegistry: registry,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	if err := s.recoverPendingTurns(context.Background()); err != nil {
		t.Fatalf("recoverPendingTurns: %v", err)
	}
	t1, _ := store.GetTurn(context.Background(), "trn_r1")
	if t1.State != "queued" {
		t.Fatalf("expected trn_r1 reset to queued, got %s", t1.State)
	}
	if registry.workerCount() != 2 {
		t.Fatalf("expected 2 workers notified, got %d", registry.workerCount())
	}
}
