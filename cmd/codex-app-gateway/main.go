package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/agentserver/agentserver/internal/codexappgateway"
	"github.com/agentserver/agentserver/internal/codexappgateway/envmcp"
)

const usage = `codex-app-gateway — codex gateway binary

Subcommands:
  env-mcp     Run as a stdio MCP child for one executor (per spawned codex turn)
  serve       Run the gateway HTTP/WS server (not implemented in this plan)
`

const envMcpHelp = `Usage: codex-app-gateway env-mcp [flags]

Run the binary as a stdio MCP child for one executor (per spawned codex turn).

Required flags:
  --exe-id     <id>             executor id
  --bridge-url <ws-url>         ws URL for /bridge/{exe_id}
  --token-env  <env-var-name>   env var holding the cap token (token never appears in argv)

Optional flags:
  --exe-desc   <text>           executor description shown to the LLM (default: --exe-id)
  --turn-id    <id>             turn id (logged to stderr only)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "env-mcp":
		runEnvMcp(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runEnvMcp(rawArgs []string) {
	args, err := parseEnvMcpArgs(rawArgs)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(os.Stderr, envMcpHelp)
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex-app-gateway env-mcp:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := envmcp.Run(ctx, args, os.Stdin, os.Stdout, os.Stderr, logger); err != nil {
		logger.Error("env-mcp exited with error", "err", err)
		os.Exit(1)
	}
}

func runServe(rawArgs []string) {
	args, err := parseServeArgs(rawArgs)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(os.Stderr, serveHelp)
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex-app-gateway serve:", err)
		os.Exit(2)
	}
	cfg, err := codexappgateway.LoadServeConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex-app-gateway serve: config:", err)
		os.Exit(2)
	}
	cfg.ListenAddr = args.ListenAddr
	if args.OperationLogURL != "" {
		cfg.OperationLogURL = args.OperationLogURL
	}
	if args.OperationLogSecret != "" {
		cfg.OperationLogSecret = args.OperationLogSecret
	}
	if args.OperationLogChan > 0 {
		cfg.OperationLogChan = args.OperationLogChan
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	selfBin, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex-app-gateway serve: resolve self path:", err)
		os.Exit(1)
	}
	srv, err := codexappgateway.NewServer(cfg, args.CodexBin, selfBin, logger)
	if err != nil {
		logger.Error("NewServer failed", "err", err)
		os.Exit(1)
	}
	if err := srv.Run(ctx, args.ListenAddr); err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("server clean exit")
}

const serveHelp = `Usage: codex-app-gateway serve [flags]

Run the codex-app-gateway HTTP/WS server: per-thread codex app-server
subprocess manager + transparent ws frame proxy. See env vars (CXG_*)
in the spec.

Flags:
  --listen-addr <addr>   HTTP listen address (default :8086, env CXG_LISTEN_ADDR)
  --codex-bin   <path>   path to the codex binary (default ` + "`" + `codex` + "`" + `, env CXG_CODEX_BIN)
`

func parseEnvMcpArgs(rawArgs []string) (envmcp.RunArgs, error) {
	fs := flag.NewFlagSet("env-mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workspaceID := fs.String("workspace-id", "", "workspace id (required)")
	execGatewayURL := fs.String("exec-gateway-url", "", "ws base URL for codex-exec-gateway /bridge (required)")
	appGatewayInternal := fs.String("app-gateway-internal", "", "http base URL for codex-app-gateway loopback (required, typically http://127.0.0.1:8086)")
	workspaceTokenEnv := fs.String("workspace-token-env", "", "env var name holding the workspace cap token (required)")
	loopbackTokenEnv := fs.String("loopback-token-env", "", "env var name holding the loopback token (required)")
	execGatewayInternalURL := fs.String("exec-gateway-internal-url", "", "http base URL for codex-exec-gateway internal API (optional; enables HTTP relay copy_path)")
	execGatewayInternalSecretEnv := fs.String("exec-gateway-internal-secret-env", "", "env var name holding the exec-gateway internal shared secret (optional)")
	if err := fs.Parse(rawArgs); err != nil {
		return envmcp.RunArgs{}, err
	}
	if fs.NArg() > 0 {
		return envmcp.RunArgs{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	for name, val := range map[string]string{
		"--workspace-id":         *workspaceID,
		"--exec-gateway-url":     *execGatewayURL,
		"--app-gateway-internal": *appGatewayInternal,
		"--workspace-token-env":  *workspaceTokenEnv,
		"--loopback-token-env":   *loopbackTokenEnv,
	} {
		if val == "" {
			return envmcp.RunArgs{}, fmt.Errorf("%s is required", name)
		}
	}
	return envmcp.RunArgs{
		WorkspaceID:               *workspaceID,
		ExecGatewayURL:            *execGatewayURL,
		AppGatewayInternal:        *appGatewayInternal,
		WorkspaceTokenEnv:         *workspaceTokenEnv,
		LoopbackTokenEnv:          *loopbackTokenEnv,
		ExecGatewayInternalURL:    *execGatewayInternalURL,
		ExecGatewayInternalSecret: *execGatewayInternalSecretEnv,
	}, nil
}
