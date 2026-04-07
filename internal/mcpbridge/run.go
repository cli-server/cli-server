package mcpbridge

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// RunMCPServer is the entry point for the `agentserver mcp-server` subcommand.
// Reads config from env vars and runs the MCP stdio server.
func RunMCPServer() {
	// Log to stderr — Claude Code captures stderr for debug, stdout is JSON-RPC.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime)

	serverURL := os.Getenv("AGENTSERVER_URL")
	token := os.Getenv("AGENTSERVER_TOKEN")
	workspaceID := os.Getenv("AGENTSERVER_WORKSPACE_ID")
	sandboxID := os.Getenv("AGENTSERVER_SANDBOX_ID")

	if serverURL == "" || token == "" || workspaceID == "" {
		fmt.Fprintln(os.Stderr, "Error: AGENTSERVER_URL, AGENTSERVER_TOKEN, and AGENTSERVER_WORKSPACE_ID are required")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		cancel()
	}()

	bridge := NewBridge(BridgeConfig{
		ServerURL:   serverURL,
		Token:       token,
		WorkspaceID: workspaceID,
		SandboxID:   sandboxID,
	})

	bridge.StartListing(ctx)

	server := NewServer(bridge.Tools(), bridge.HandleTool)

	log.Printf("agentserver mcp-server started (server=%s, workspace=%s)", serverURL, workspaceID)

	if err := server.Run(); err != nil {
		log.Fatalf("mcp server error: %v", err)
	}
}
