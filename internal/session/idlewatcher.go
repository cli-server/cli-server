package session

import (
	"log"
	"time"

	"github.com/imryao/cli-server/internal/db"
	"github.com/imryao/cli-server/internal/process"
)

// IdleWatcher monitors sessions and auto-pauses idle ones.
type IdleWatcher struct {
	db      *db.DB
	procMgr process.Manager
	store   *Store
	timeout time.Duration
	stop    chan struct{}
}

// NewIdleWatcher creates a new idle session watcher.
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
	sessions, err := w.db.ListIdleSessions(w.timeout)
	if err != nil {
		log.Printf("idle watcher: failed to list idle sessions: %v", err)
		return
	}

	for _, sess := range sessions {
		log.Printf("idle watcher: pausing idle session %s (last activity: %v)", sess.ID, sess.LastActivityAt)

		// Transition to pausing.
		if err := w.store.UpdateStatus(sess.ID, StatusPausing); err != nil {
			log.Printf("idle watcher: failed to set pausing status for %s: %v", sess.ID, err)
			continue
		}

		// Pause the process.
		if err := w.procMgr.Pause(sess.ID); err != nil {
			log.Printf("idle watcher: failed to pause process for %s: %v", sess.ID, err)
			// Revert status to running.
			w.store.UpdateStatus(sess.ID, StatusRunning)
			continue
		}

		// Transition to paused.
		if err := w.store.UpdateStatus(sess.ID, StatusPaused); err != nil {
			log.Printf("idle watcher: failed to set paused status for %s: %v", sess.ID, err)
		}
	}
}
