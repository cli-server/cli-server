package envmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// RunArgs is the parsed CLI input for `codex-app-gateway env-mcp`.
type RunArgs struct {
	ExeID     string
	BridgeURL string
	TokenEnv  string
	ExeDesc   string
	TurnID    string
}

// Run dials the bridge, initializes the exec-server session, then runs
// the stdio MCP server loop until stdin EOF or context cancellation.
//
// stderr is reserved for env-mcp's own diagnostic logging; stdout is
// dedicated to MCP JSON-RPC frames. Anything written to stdout outside
// of MCPServer.Serve corrupts the MCP stream.
func Run(ctx context.Context, args RunArgs, stdin io.Reader, stdout, _ io.Writer, logger *slog.Logger) error {
	token := os.Getenv(args.TokenEnv)
	if token == "" {
		return fmt.Errorf("env var %s is empty; cannot authenticate to bridge", args.TokenEnv)
	}

	logger.Info("env-mcp starting",
		"exe_id", args.ExeID,
		"bridge_url", args.BridgeURL,
		"turn_id", args.TurnID,
	)

	bc, err := DialBridge(ctx, args.BridgeURL, token)
	if err != nil {
		return fmt.Errorf("dial bridge: %w", err)
	}
	defer bc.Close()

	initParams, _ := json.Marshal(ExecInitializeParams{ClientName: "codex-env-mcp"})
	if _, err := bc.Call(ctx, ExecMethodInitialize, initParams); err != nil {
		return fmt.Errorf("exec-server initialize: %w", err)
	}
	if err := bc.Notify(ctx, ExecMethodInitialized, nil); err != nil {
		return fmt.Errorf("exec-server initialized notify: %w", err)
	}

	tr := NewTranslator(bc)
	srv := NewMCPServer(args.ExeDesc, tr)
	if err := srv.Serve(ctx, stdin, stdout); err != nil {
		// EOF on stdin is the normal exit path (codex's MCP host
		// closed); io.EOF surfaces as nil from bufio.Scanner.Err(), so
		// any non-nil error here is genuinely abnormal.
		return fmt.Errorf("mcp serve: %w", err)
	}
	logger.Info("env-mcp clean exit (stdin closed)")
	return nil
}
