package ccbroker

import (
	"context"
	"sync"
	"time"
)

// workerRegistry owns the per-session sessionWorker pool. Producers call
// Notify(sid); the registry spawns a worker if none exists, otherwise
// signals the existing one. Workers self-unregister via onIdleExit.
type workerRegistry struct {
	mu          sync.Mutex
	workers     map[string]*sessionWorker
	deps        workerDeps
	ctx         context.Context
	cancel      context.CancelFunc
	idleTimeout time.Duration

	// executeOverride lets tests stub the heavy path. Production leaves nil.
	executeOverride func(ctx context.Context, t *AgentTurn)
}

func newWorkerRegistry(deps workerDeps) *workerRegistry {
	ctx, cancel := context.WithCancel(context.Background())
	return &workerRegistry{
		workers:     make(map[string]*sessionWorker),
		deps:        deps,
		ctx:         ctx,
		cancel:      cancel,
		idleTimeout: defaultWorkerIdleTimeout,
	}
}

func (r *workerRegistry) Notify(sessionID string) {
	r.mu.Lock()
	w, ok := r.workers[sessionID]
	if !ok {
		w = newSessionWorker(sessionID, r.deps, r.onIdleExit)
		w.idleAfter = r.idleTimeout
		if r.executeOverride != nil {
			w.executeFn = r.executeOverride
		}
		r.workers[sessionID] = w
		go w.run(r.ctx)
	}
	r.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (r *workerRegistry) onIdleExit(sessionID string) {
	r.mu.Lock()
	delete(r.workers, sessionID)
	r.mu.Unlock()
}

func (r *workerRegistry) workerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.workers)
}

func (r *workerRegistry) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	workers := make([]*sessionWorker, 0, len(r.workers))
	for _, w := range r.workers {
		workers = append(workers, w)
	}
	r.mu.Unlock()
	for _, w := range workers {
		select {
		case <-w.quit:
		default:
			close(w.quit)
		}
	}
	r.cancel()

	deadline := time.Now().Add(2 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for time.Now().Before(deadline) {
		if r.workerCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	r.workers = map[string]*sessionWorker{}
	r.mu.Unlock()
	return nil
}
