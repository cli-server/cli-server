package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ClaudeCodeOptions holds all flags for the claudecode command.
type ClaudeCodeOptions struct {
	Server          string
	Name            string
	SkipOpenBrowser bool
	ClaudeBin       string
	WorkDir         string
	Resume          string // sandbox ID to resume
	Continue        bool   // resume most recent session
}

// RunClaudeCode executes the Claude Code agent connect workflow.
// Each invocation registers a new sandbox (or resumes an existing one),
// then establishes a tunnel and bridges terminal streams to a local Claude Code PTY.
func RunClaudeCode(opts ClaudeCodeOptions) {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}
	if opts.WorkDir == "" {
		opts.WorkDir = cwd
	}
	if opts.Name == "" {
		hostname, _ := os.Hostname()
		if hostname != "" {
			opts.Name = hostname
		} else {
			opts.Name = "Claude Code Agent"
		}
	}

	var session *Session

	// Resume mode: load an existing session.
	if opts.Resume != "" {
		s, err := LoadSession(opts.Resume)
		if err != nil {
			log.Fatalf("Failed to load session: %v", err)
		}
		if isProcessAlive(s.PID) {
			log.Fatalf("Session %s is still active (PID %d)", s.SandboxID, s.PID)
		}
		session = s
		opts.Server = s.ServerURL
		log.Printf("Resuming session (sandbox: %s)", session.SandboxID)
	} else if opts.Continue {
		s, err := FindLatestSession(cwd, "claudecode")
		if err != nil {
			log.Fatalf("No session to continue: %v", err)
		}
		session = s
		opts.Server = s.ServerURL
		log.Printf("Continuing session (sandbox: %s)", session.SandboxID)
	}

	// New session: ensure login, register new sandbox.
	if session == nil {
		if opts.Server == "" {
			// Try to get server URL from saved credentials.
			creds, _ := LoadCredentials(DefaultCredentialsPath())
			if creds != nil && creds.ServerURL != "" {
				opts.Server = creds.ServerURL
			} else {
				log.Fatal("--server is required (no saved credentials found)")
			}
		}

		// Ensure we have a valid access token.
		accessToken, err := EnsureValidToken(opts.Server)
		if err != nil {
			// Need interactive login.
			log.Println("No valid credentials, starting login...")
			if err := RunLogin(LoginOptions{
				ServerURL:       opts.Server,
				SkipOpenBrowser: opts.SkipOpenBrowser,
			}); err != nil {
				log.Fatalf("Login failed: %v", err)
			}
			accessToken, err = EnsureValidToken(opts.Server)
			if err != nil {
				log.Fatalf("Failed to get access token after login: %v", err)
			}
		}

		// Register a new sandbox.
		regResp, err := registerAgentWithToken(opts.Server, accessToken, opts.Name, "claudecode")
		if err != nil {
			log.Fatalf("Agent registration failed: %v", err)
		}
		log.Printf("Registered new sandbox: %s", regResp.SandboxID)

		session = &Session{
			SandboxID:   regResp.SandboxID,
			TunnelToken: regResp.TunnelToken,
			ProxyToken:  regResp.ProxyToken,
			WorkspaceID: regResp.WorkspaceID,
			Name:        opts.Name,
			Type:        "claudecode",
			ServerURL:   opts.Server,
			Dir:         cwd,
			CreatedAt:   time.Now(),
		}
	}

	// Save session with current PID.
	if err := SaveSession(session); err != nil {
		log.Printf("Warning: failed to save session: %v", err)
	}
	defer CleanupSession(session)

	// PTY management: start Claude Code lazily on first terminal stream.
	var ptyMu sync.Mutex
	var ptyInstance *ClaudeCodePTY

	tunnelClient := NewClient(session.ServerURL, session.SandboxID, session.TunnelToken, "", "", opts.WorkDir)
	tunnelClient.BackendType = "claudecode"

	// Set up terminal stream handler.
	tunnelClient.OnTerminalStream = func(stream net.Conn) {
		ptyMu.Lock()
		if ptyInstance != nil && !ptyInstance.IsAlive() {
			log.Printf("Claude Code PTY exited, will restart on next connection")
			ptyInstance.Close()
			ptyInstance = nil
		}
		if ptyInstance == nil {
			log.Printf("Starting Claude Code PTY...")
			claudeBin := opts.ClaudeBin
			if claudeBin == "" {
				claudeBin = "claude"
			}
			var err error
			ptyInstance, err = NewClaudeCodePTY(claudeBin, opts.WorkDir, 120, 40)
			if err != nil {
				ptyMu.Unlock()
				log.Printf("Failed to start Claude Code: %v", err)
				stream.Close()
				return
			}
			log.Printf("Claude Code PTY started")
		}
		p := ptyInstance
		ptyMu.Unlock()

		BridgeTerminalStream(stream, p)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("Received %v, disconnecting...", sig)
		cancel()
		ptyMu.Lock()
		if ptyInstance != nil {
			ptyInstance.Close()
		}
		ptyMu.Unlock()
	}()

	// Auto-register agent card.
	if err := RegisterDefaultCard(session.ServerURL, session.TunnelToken, session.Name); err != nil {
		log.Printf("Warning: failed to register agent card: %v (will retry on reconnect)", err)
	} else {
		log.Printf("Agent card registered: %s", session.Name)
	}

	// Inject MCP bridge config.
	if err := injectMCPConfig(session.ServerURL, session.TunnelToken, session.WorkspaceID, session.SandboxID, opts.WorkDir); err != nil {
		log.Printf("Warning: failed to inject MCP config: %v", err)
	} else {
		log.Printf("MCP bridge config injected")
	}

	// Start task worker in background.
	go RunTaskWorker(ctx, TaskWorkerOptions{
		ServerURL:  session.ServerURL,
		ProxyToken: session.TunnelToken,
		SandboxID:  session.SandboxID,
		Workdir:    opts.WorkDir,
		CLIPath:    opts.ClaudeBin,
	})

	log.Printf("Connecting to %s (Claude Code terminal agent)...", session.ServerURL)
	if err := tunnelClient.Run(ctx); err != nil && ctx.Err() == nil {
		CleanupSession(session)
		log.Fatalf("Agent error: %v", err)
	}

	ptyMu.Lock()
	if ptyInstance != nil {
		ptyInstance.Close()
	}
	ptyMu.Unlock()

	fmt.Println("Claude Code agent disconnected.")
	fmt.Printf("To resume this session: agentserver --resume %s\n", session.SandboxID)
}

// injectMCPConfig writes a .mcp.json in the working directory so Claude Code
// auto-discovers the agentserver MCP bridge.
func injectMCPConfig(serverURL, token, workspaceID, sandboxID, workDir string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find own executable: %w", err)
	}
	self, _ = filepath.EvalSymlinks(self)

	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"agentserver": map[string]any{
				"command": self,
				"args":    []string{"mcp-server"},
				"env": map[string]string{
					"AGENTSERVER_URL":          serverURL,
					"AGENTSERVER_TOKEN":        token,
					"AGENTSERVER_WORKSPACE_ID": workspaceID,
					"AGENTSERVER_SANDBOX_ID":   sandboxID,
				},
			},
		},
	}

	mcpPath := filepath.Join(workDir, ".mcp.json")
	existing := make(map[string]any)
	if data, err := os.ReadFile(mcpPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	existingServers, _ := existing["mcpServers"].(map[string]any)
	if existingServers == nil {
		existingServers = make(map[string]any)
	}
	newServers := mcpConfig["mcpServers"].(map[string]any)
	for k, v := range newServers {
		existingServers[k] = v
	}
	existing["mcpServers"] = existingServers

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(mcpPath, append(data, '\n'), 0600)
}
