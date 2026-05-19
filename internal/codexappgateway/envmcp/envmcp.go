// Package envmcp implements the `codex-app-gateway env-mcp` subcommand:
// a stateless MCP server that codex spawns as a child process. It
// exposes a fixed tool set (list_environments, shell, exec_command,
// write_stdin, read_output, terminate, read_file, apply_patch) to
// codex; tool calls are multiplexed across the workspace's connected
// executors via a per-exe BridgeClient pool keyed by environment name.
package envmcp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/agentserver/agentserver/internal/envtools/bridge"
	"github.com/agentserver/agentserver/internal/envtools/nameresolver"
	"github.com/agentserver/agentserver/internal/envtools/tools"
)

// RunArgs is the parsed CLI input for `codex-app-gateway env-mcp`.
// Per the 2026-05-16 fixed-tools redesign, env-mcp is workspace-scoped
// rather than per-executor; one child binary handles every executor in
// the workspace via environment_id routing.
type RunArgs struct {
	WorkspaceID        string // --workspace-id
	ExecGatewayURL     string // --exec-gateway-url; pool appends /<exe_id>
	AppGatewayInternal string // --app-gateway-internal; list_environments calls /internal/connected here
	WorkspaceTokenEnv  string // --workspace-token-env (workspace-scoped cap token)
	LoopbackTokenEnv   string // --loopback-token-env (for /internal/connected)
	// ExecGatewayInternalURL is the http(s):// base for codex-exec-gateway's
	// internal API (NOT the ws /bridge URL). copy_path's HTTP relay path
	// POSTs to <base>/api/exec-gateway/relay/create here. Empty disables
	// the HTTP relay path; copy_path falls back to the ws cat-pump.
	ExecGatewayInternalURL    string // --exec-gateway-internal-url
	ExecGatewayInternalSecret string // --exec-gateway-internal-secret-env (env var name; value injected by gateway)
}

// Run constructs the BridgePool, builds the tool registry, and serves
// the MCP loop on stdin/stdout until EOF or context cancellation.
//
// stdout is the MCP JSON-RPC stream; do not write to it from outside
// MCPServer.Serve. Diagnostic output flows through logger (gateway
// supervisor pipes our stderr into the pod's stderr with a
// `[codex-subproc]` prefix). The `stderr` parameter is reserved for
// future direct writes (e.g., panic dumps) and currently unused.
func Run(ctx context.Context, args RunArgs, stdin io.Reader, stdout, stderr io.Writer, logger *slog.Logger) error {
	_ = stderr
	wsToken := os.Getenv(args.WorkspaceTokenEnv)
	if wsToken == "" {
		return fmt.Errorf("env var %s is empty; cannot authenticate to bridge", args.WorkspaceTokenEnv)
	}
	lbToken := os.Getenv(args.LoopbackTokenEnv)
	if lbToken == "" {
		return fmt.Errorf("env var %s is empty; cannot authenticate to app-gateway loopback", args.LoopbackTokenEnv)
	}
	if args.WorkspaceID == "" || args.ExecGatewayURL == "" || args.AppGatewayInternal == "" {
		return fmt.Errorf("env-mcp: workspace-id, exec-gateway-url, app-gateway-internal all required")
	}
	// ExecGatewayInternalSecret is optional — if its env var holds a value,
	// copy_path can use the HTTPS relay path; otherwise it falls back to
	// the ws cat-pump. We resolve here so the tool sees the value directly.
	var execGwSecret string
	if args.ExecGatewayInternalSecret != "" {
		execGwSecret = os.Getenv(args.ExecGatewayInternalSecret)
	}

	logger.Info("env-mcp starting",
		"workspace_id", args.WorkspaceID,
		"exec_gateway_url", args.ExecGatewayURL,
		"app_gateway_internal", args.AppGatewayInternal,
		"exec_gateway_internal_url", args.ExecGatewayInternalURL,
		"http_relay_enabled", args.ExecGatewayInternalURL != "" && execGwSecret != "",
	)

	pool := bridge.NewPool(args.ExecGatewayURL, wsToken, logger)
	defer pool.Close()

	sessions := tools.NewSessionStore()
	connectedURL := strings.TrimRight(args.AppGatewayInternal, "/") + "/internal/connected"
	resolver := nameresolver.NewResolver(connectedURL, lbToken, logger)

	relayClient := bridge.NewRelayClient(args.ExecGatewayInternalURL, execGwSecret, args.WorkspaceID, logger)
	toolList := []tools.Tool{
		tools.NewListEnvironmentsTool(resolver),
		tools.NewShellTool(pool, resolver),
		tools.NewUnifiedExecTool(pool, sessions, resolver),
		tools.NewWriteStdinTool(pool, sessions),
		tools.NewReadOutputTool(pool, sessions),
		tools.NewTerminateTool(pool, sessions),
		tools.NewReadFileTool(pool, resolver),
		tools.NewApplyPatchTool(pool, resolver),
		tools.NewCopyPathTool(pool, resolver, relayClient),
	}
	srv := NewMCPServer("agentserver", toolList, logger)
	if err := srv.Serve(ctx, stdin, stdout); err != nil {
		return fmt.Errorf("mcp serve: %w", err)
	}
	logger.Info("env-mcp clean exit (stdin closed)")
	return nil
}
