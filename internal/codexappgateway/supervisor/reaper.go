package supervisor

import (
	"context"
	"time"
)

// IdleReaper periodically scans the Supervisor and shuts down entries
// idle for longer than idleAfter.
type IdleReaper struct {
	sup       *Supervisor
	interval  time.Duration
	idleAfter time.Duration
}

func NewIdleReaper(sup *Supervisor, interval, idleAfter time.Duration) *IdleReaper {
	return &IdleReaper{sup: sup, interval: interval, idleAfter: idleAfter}
}

// Run blocks until ctx is done, ticking every interval and shutting
// down idle entries.
func (r *IdleReaper) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for key, last := range r.sup.snapshot() {
				if now.Sub(last) >= r.idleAfter {
					_ = r.sup.Shutdown(ctx, key)
				}
			}
		}
	}
}
