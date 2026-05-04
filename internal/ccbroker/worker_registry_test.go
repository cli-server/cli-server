package ccbroker

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryNotifySpawnsThenSignals(t *testing.T) {
	deps := workerDeps{
		store:  newFakeStore(),
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	r := newWorkerRegistry(deps)
	defer r.Shutdown(context.Background())

	r.Notify("sess_a")
	if r.workerCount() != 1 {
		t.Fatalf("expected 1 worker, got %d", r.workerCount())
	}
	r.Notify("sess_a")
	if r.workerCount() != 1 {
		t.Fatalf("Notify should be idempotent; got %d", r.workerCount())
	}
	r.Notify("sess_b")
	if r.workerCount() != 2 {
		t.Fatalf("expected 2 workers, got %d", r.workerCount())
	}
}

func TestRegistryShutdownClosesWorkers(t *testing.T) {
	store := newFakeStore()
	var executed atomic.Int32
	deps := workerDeps{
		store:  store,
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	r := newWorkerRegistry(deps)
	r.executeOverride = func(_ context.Context, _ *AgentTurn) { executed.Add(1) }
	r.Notify("sess_s")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if r.workerCount() != 0 {
		t.Fatalf("expected 0 workers after Shutdown, got %d", r.workerCount())
	}
}

func TestRegistryOnIdleExitUnregisters(t *testing.T) {
	deps := workerDeps{
		store:  newFakeStore(),
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	r := newWorkerRegistry(deps)
	r.idleTimeout = 50 * time.Millisecond
	r.Notify("sess_i")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.workerCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker did not idle-exit; count=%d", r.workerCount())
}
