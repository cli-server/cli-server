// agentserver-mcp-bridge is a stdio MCP server that exposes agentserver
// agent discovery and task delegation as tools for Claude Code.
//
// Configuration via environment variables:
//
//	AGENTSERVER_URL          - agentserver base URL (required)
//	AGENTSERVER_TOKEN        - tunnel_token or proxy_token (required)
//	AGENTSERVER_WORKSPACE_ID - workspace ID (required)
//	AGENTSERVER_SANDBOX_ID   - this agent's sandbox ID (optional, excluded from discovery)
//
// Usage in Claude Code MCP config (.mcp.json):
//
//	{
//	  "mcpServers": {
//	    "agentserver": {
//	      "command": "agentserver-mcp-bridge",
//	      "env": {
//	        "AGENTSERVER_URL": "https://agent.cs.ac.cn",
//	        "AGENTSERVER_TOKEN": "<tunnel_token>",
//	        "AGENTSERVER_WORKSPACE_ID": "<workspace_id>",
//	        "AGENTSERVER_SANDBOX_ID": "<sandbox_id>"
//	      }
//	    }
//	  }
//	}
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentserver/agentserver/internal/mcpbridge"
)

func main() {
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

	bridge := mcpbridge.NewBridge(mcpbridge.BridgeConfig{
		ServerURL:   serverURL,
		Token:       token,
		WorkspaceID: workspaceID,
		SandboxID:   sandboxID,
	})

	// Start agent listing refresh in background.
	bridge.StartListing(ctx)

	// Create and run MCP server.
	server := mcpbridge.NewServer(bridge.Tools(), bridge.HandleTool)

	log.Printf("agentserver-mcp-bridge started (server=%s, workspace=%s)", serverURL, workspaceID)

	if err := server.Run(); err != nil {
		log.Fatalf("mcp server error: %v", err)
	}
}
