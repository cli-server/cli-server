package sbxstore

import (
	"log"
	"time"

	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/process"
)

// IdleWatcher monitors sandboxes and auto-pauses idle ones.
type IdleWatcher struct {
	db      *db.DB
	procMgr process.Manager
	store   *Store
	timeout time.Duration
	stop    chan struct{}
}

// NewIdleWatcher creates a new idle sandbox watcher.
func NewIdleWatcher(database *db.DB, procMgr process.Manager, store *Store, timeout time.Duration) *IdleWatcher {
	return &IdleWatcher{
		db:      database,
		procMgr: procMgr,
		store:   store,
		timeout: timeout,
		stop:    make(chan struct{}),
	}
}

// Start begins the idle check loop. Call Stop() to terminate.
func (w *IdleWatcher) Start() {
	go w.loop()
}

// Stop terminates the idle watcher.
func (w *IdleWatcher) Stop() {
	close(w.stop)
}

func (w *IdleWatcher) loop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.check()
		}
	}
}

func (w *IdleWatcher) check() {
	sandboxes, err := w.db.ListIdleSandboxes(w.timeout)
	if err != nil {
		log.Printf("idle watcher: failed to list idle sandboxes: %v", err)
		return
	}

	for _, sbx := range sandboxes {
		log.Printf("idle watcher: pausing idle sandbox %s (last activity: %v)", sbx.ID, sbx.LastActivityAt)

		// Transition to pausing.
		if err := w.store.UpdateStatus(sbx.ID, StatusPausing); err != nil {
			log.Printf("idle watcher: failed to set pausing status for %s: %v", sbx.ID, err)
			continue
		}

		// Pause the process.
		if err := w.procMgr.Pause(sbx.ID); err != nil {
			log.Printf("idle watcher: failed to pause process for %s: %v", sbx.ID, err)
			// Revert status to running.
			w.store.UpdateStatus(sbx.ID, StatusRunning)
			continue
		}

		// Clear pod IP so the proxy won't connect to a stale address.
		if err := w.db.UpdateSandboxPodIP(sbx.ID, ""); err != nil {
			log.Printf("idle watcher: failed to clear pod IP for %s: %v", sbx.ID, err)
		}

		// Transition to paused.
		if err := w.store.UpdateStatus(sbx.ID, StatusPaused); err != nil {
			log.Printf("idle watcher: failed to set paused status for %s: %v", sbx.ID, err)
		}
	}
}
