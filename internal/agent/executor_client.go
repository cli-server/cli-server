package agent

import (
	"context"
	"log"
	"time"
)

// ExecutorClient runs a tunnel to executor-registry and serves tool
// execution requests from cc-broker workers.
//
// Task 1 skeleton — the real tunnel + heartbeat + dispatch logic is
// filled in by Task 2 (tunnel) and Task 3 (tool dispatch).
type ExecutorClient struct {
	session *ExecutorSession
	workDir string
}

// NewExecutorClient constructs a new executor client bound to the given
// registry session and working directory.
func NewExecutorClient(sess *ExecutorSession, workDir string) *ExecutorClient {
	return &ExecutorClient{
		session: sess,
		workDir: workDir,
	}
}

// Run blocks until the context is cancelled. Task 2 replaces this stub
// with the real tunnel accept loop + heartbeat goroutine.
func (c *ExecutorClient) Run(ctx context.Context) error {
	log.Printf("executor client: tunnel/heartbeat not yet implemented; idling until Ctrl+C")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
			log.Printf("executor client: idle (session=%s)", c.session.ExecutorID)
		}
	}
}
