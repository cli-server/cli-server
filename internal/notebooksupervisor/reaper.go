package notebooksupervisor

import (
	"context"
	"time"
)

// StartReaper runs the idle reap loop. Returns when ctx is cancelled.
// Each tick: snapshot idle Keys (lastActive + IdleTTL < now), call
// Stop on each. Errors are logged, never propagated.
func (s *Supervisor) StartReaper(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapOnce(ctx)
		}
	}
}

func (s *Supervisor) reapOnce(ctx context.Context) {
	cutoff := time.Now().Add(-s.cfg.IdleTTL)
	s.mu.Lock()
	idle := s.idleKeys(cutoff)
	s.mu.Unlock()
	for _, k := range idle {
		if err := s.Stop(ctx, k); err != nil {
			s.logger.Warn("notebooksupervisor: reap stop failed", "key", k, "err", err)
		} else {
			s.logger.Info("notebooksupervisor: reaped idle deployment", "key", k)
		}
	}
}
