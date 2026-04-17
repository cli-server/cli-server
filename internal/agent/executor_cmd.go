package agent

import (
	"context"
	"fmt"
	"log"
	"os"
)

// ExecutorOpts holds configuration for the `executor` subcommand.
type ExecutorOpts struct {
	ServerURL       string
	Name            string
	WorkspaceID     string
	SkipOpenBrowser bool
	WorkDir         string
}

// RunExecutor registers with executor-registry (or reuses a saved session)
// and runs the tunnel + heartbeat loop, serving tool execution requests
// over a yamux tunnel.
func RunExecutor(ctx context.Context, opts ExecutorOpts) error {
	if opts.WorkspaceID == "" {
		return fmt.Errorf("--workspace-id is required")
	}
	if opts.ServerURL == "" {
		return fmt.Errorf("--registry is required")
	}

	workDir := opts.WorkDir
	if workDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		workDir = cwd
	}

	sess, err := LoadOrRegisterExecutor(opts)
	if err != nil {
		return fmt.Errorf("register executor: %w", err)
	}

	log.Printf("executor registered: id=%s name=%s", sess.ExecutorID, sess.Name)
	log.Printf("registry URL: %s", sess.ServerURL)
	log.Printf("working directory: %s", workDir)

	client := NewExecutorClient(sess, workDir)
	return client.Run(ctx)
}
