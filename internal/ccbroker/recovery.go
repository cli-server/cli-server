package ccbroker

import (
	"context"
	"fmt"
)

// recoverPendingTurns is called once at Server startup before HTTP serving.
// It finds turns left in 'running' state by a crashed prior pod, resets
// them to 'queued', then notifies one worker per session that has any
// pending work so the queue drains immediately.
func (s *Server) recoverPendingTurns(ctx context.Context) error {
	n, err := s.store.ResetRunningToQueued(ctx)
	if err != nil {
		return fmt.Errorf("reset running to queued: %w", err)
	}
	if n > 0 {
		s.logger.Info("recovery: reset stale running turns", "count", n)
	}
	sids, err := s.store.ListSessionsWithPending(ctx)
	if err != nil {
		return fmt.Errorf("list sessions with pending: %w", err)
	}
	for _, sid := range sids {
		s.workerRegistry.Notify(sid)
	}
	s.logger.Info("recovery: notified workers", "session_count", len(sids))
	return nil
}
