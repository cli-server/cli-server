package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
)

// ClaudeCodeOptions holds all flags for the claudecode command.
type ClaudeCodeOptions struct {
	Server    string
	Code      string
	Name      string
	ClaudeBin string
	WorkDir   string
}

// RunClaudeCode executes the Claude Code agent connect workflow.
// It registers with the server (or loads saved credentials), then establishes
// a tunnel and bridges terminal streams to a local Claude Code PTY.
func RunClaudeCode(opts ClaudeCodeOptions) {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}
	if opts.WorkDir == "" {
		opts.WorkDir = cwd
	}

	registryPath := DefaultRegistryPath()

	locked, err := LockRegistry(registryPath)
	if err != nil {
		log.Fatalf("Failed to load registry: %v", err)
	}
	defer locked.Close()

	reg := locked.Reg
	var entry *RegistryEntry

	if opts.Code != "" {
		// New registration.
		if opts.Server == "" {
			log.Fatal("--server is required for registration")
		}
		if opts.Name == "" {
			hostname, _ := os.Hostname()
			if hostname != "" {
				opts.Name = hostname
			} else {
				opts.Name = "Claude Code Agent"
			}
		}

		log.Printf("Registering with server %s...", opts.Server)
		entry, err = Register(opts.Server, opts.Code, opts.Name, "claudecode")
		if err != nil {
			log.Fatalf("Registration failed: %v", err)
		}
		log.Printf("Registered successfully (sandbox: %s)", entry.SandboxID)

		entry.Dir = cwd

		if existing := reg.Find(cwd, entry.WorkspaceID); existing != nil {
			log.Printf("Warning: overwriting existing entry for dir=%q workspace=%q", cwd, entry.WorkspaceID)
		}

		reg.Put(entry)
		if err := locked.Save(); err != nil {
			log.Printf("Warning: failed to save registry: %v", err)
		}
	} else {
		// Reconnect using saved credentials.
		entries := reg.FindByDir(cwd)
		// Filter to claudecode entries only.
		var ccEntries []*RegistryEntry
		for _, e := range entries {
			if e.Type == "claudecode" {
				ccEntries = append(ccEntries, e)
			}
		}
		switch len(ccEntries) {
		case 0:
			log.Fatal("No Claude Code agent registered for this directory. Use --code to register.")
		case 1:
			entry = ccEntries[0]
		default:
			log.Printf("Multiple Claude Code agents registered for this directory:")
			for _, e := range ccEntries {
				log.Printf("  workspace=%s  name=%s  sandbox=%s", e.WorkspaceID, e.Name, e.SandboxID)
			}
			log.Fatal("Use --code to create a new registration or remove duplicates.")
		}
		log.Printf("Using saved credentials (sandbox: %s)", entry.SandboxID)
		if opts.Server != "" {
			entry.Server = opts.Server
		}
	}

	// PTY management: start Claude Code lazily on first terminal stream.
	var ptyMu sync.Mutex
	var ptyInstance *ClaudeCodePTY

	tunnelClient := NewClient(entry.Server, entry.SandboxID, entry.TunnelToken, "", "", opts.WorkDir)
	tunnelClient.BackendType = "claudecode"

	// Set up terminal stream handler.
	tunnelClient.OnTerminalStream = func(stream net.Conn) {
		ptyMu.Lock()
		// Reset dead PTY instance so a new one is started.
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
	if err := RegisterDefaultCard(entry.Server, entry.TunnelToken, entry.Name); err != nil {
		log.Printf("Warning: failed to register agent card: %v (will retry on reconnect)", err)
	} else {
		log.Printf("Agent card registered: %s", entry.Name)
	}

	// Inject MCP bridge config so Claude Code auto-discovers agentserver tools.
	if err := injectMCPConfig(entry.Server, entry.TunnelToken, entry.WorkspaceID, entry.SandboxID, opts.WorkDir); err != nil {
		log.Printf("Warning: failed to inject MCP config: %v", err)
	} else {
		log.Printf("MCP bridge config injected")
	}

	// Start task worker in background.
	go RunTaskWorker(ctx, TaskWorkerOptions{
		ServerURL:  entry.Server,
		ProxyToken: entry.TunnelToken,
		SandboxID:  entry.SandboxID,
		Workdir:    opts.WorkDir,
		CLIPath:    opts.ClaudeBin,
	})

	log.Printf("Connecting to %s (Claude Code terminal agent)...", entry.Server)
	if err := tunnelClient.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("Agent error: %v", err)
	}

	ptyMu.Lock()
	if ptyInstance != nil {
		ptyInstance.Close()
	}
	ptyMu.Unlock()

	fmt.Println("Claude Code agent disconnected.")
}

// injectMCPConfig writes a .mcp.json in the working directory so Claude Code
// auto-discovers the agentserver MCP bridge.
func injectMCPConfig(serverURL, token, workspaceID, sandboxID, workDir string) error {
	// Find the MCP bridge binary.
	bridgeBin, err := findMCPBridge()
	if err != nil {
		return fmt.Errorf("agentserver-mcp-bridge not found: %w", err)
	}

	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"agentserver": map[string]any{
				"command": bridgeBin,
				"env": map[string]string{
					"AGENTSERVER_URL":          serverURL,
					"AGENTSERVER_TOKEN":        token,
					"AGENTSERVER_WORKSPACE_ID": workspaceID,
					"AGENTSERVER_SANDBOX_ID":   sandboxID,
				},
			},
		},
	}

	// Read existing .mcp.json and merge (don't overwrite user's other MCP servers).
	mcpPath := filepath.Join(workDir, ".mcp.json")
	existing := make(map[string]any)
	if data, err := os.ReadFile(mcpPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	// Merge mcpServers.
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
	return os.WriteFile(mcpPath, append(data, '\n'), 0644)
}

// findMCPBridge locates the agentserver-mcp-bridge binary.
func findMCPBridge() (string, error) {
	// 1. Same directory as current executable.
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "agentserver-mcp-bridge")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	// 2. PATH.
	if path, err := exec.LookPath("agentserver-mcp-bridge"); err == nil {
		return path, nil
	}

	// 3. Common locations.
	for _, p := range []string{
		"/usr/local/bin/agentserver-mcp-bridge",
		"/opt/homebrew/bin/agentserver-mcp-bridge",
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("not in PATH or standard locations")
}
