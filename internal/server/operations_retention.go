package server

import (
	"context"
	"log"
	"time"
)

// runRetentionOnce deletes operations older than ttl. Exposed for tests
// and for an admin "prune now" code path if needed later.
func (s *Server) runRetentionOnce(ttl time.Duration) (int64, error) {
	cutoff := time.Now().Add(-ttl)
	return s.DB.PruneOperationsOlderThan(cutoff)
}

// StartRetentionLoop is the exported entry point for the server's main
// lifecycle to launch the retention loop in a goroutine.
func (s *Server) StartRetentionLoop(ctx context.Context, ttl time.Duration, every time.Duration) {
	s.startRetentionLoop(ctx, ttl, every)
}

// startRetentionLoop ticks every `every` and prunes operations older
// than ttl. Returns when ctx is cancelled. Errors are logged, not
// propagated — a transient PG failure shouldn't kill the loop.
//
// ttl <= 0 disables the loop (returns immediately).
func (s *Server) startRetentionLoop(ctx context.Context, ttl time.Duration, every time.Duration) {
	if ttl <= 0 {
		return
	}
	if every <= 0 {
		every = time.Hour
	}
	log.Printf("operations retention loop: ttl=%s interval=%s", ttl, every)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.runRetentionOnce(ttl)
			if err != nil {
				log.Printf("operations retention: prune failed: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("operations retention: pruned %d rows older than %s", n, ttl)
			}
		}
	}
}
