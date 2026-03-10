package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ConnectOptions holds all flags for the connect command.
type ConnectOptions struct {
	Server          string
	Code            string
	Name            string
	WorkspaceID     string // optional: disambiguate when dir has multiple workspaces
	OpencodeURL     string
	OpencodeURLSet  bool // true if --opencode-url was explicitly provided
	OpencodeToken   string
	AutoStart       bool
	OpencodeBin     string
	OpencodePort    int  // 0 = auto-assign from registry
	OpencodePortSet bool // true if --opencode-port was explicitly provided
}

// RunConnect executes the agent connect workflow.
func RunConnect(opts ConnectOptions) {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}

	// Load registry.
	registryPath := DefaultRegistryPath()
	reg, err := LoadRegistry(registryPath)
	if err != nil {
		log.Fatalf("Failed to load registry: %v", err)
	}

	// Attempt legacy migration.
	legacyPath := DefaultConfigPath()
	migrated, err := MaybeMigrateLegacy(legacyPath, registryPath, cwd)
	if err != nil {
		log.Printf("Warning: legacy migration failed: %v", err)
	}
	if migrated != nil {
		reg = migrated
		log.Printf("Migrated legacy config to registry")
	}

	var entry *RegistryEntry

	if opts.Code != "" {
		// --- New registration ---
		if opts.Server == "" {
			log.Fatal("--server is required for registration")
		}
		if opts.Name == "" {
			hostname, _ := os.Hostname()
			if hostname != "" {
				opts.Name = hostname
			} else {
				opts.Name = "Local Agent"
			}
		}

		log.Printf("Registering with server %s...", opts.Server)
		entry, err = Register(opts.Server, opts.Code, opts.Name)
		if err != nil {
			log.Fatalf("Registration failed: %v", err)
		}
		log.Printf("Registered successfully (sandbox: %s)", entry.SandboxID)

		entry.Dir = cwd

		// Assign port.
		if opts.OpencodePortSet {
			entry.OpencodePort = opts.OpencodePort
		} else {
			entry.OpencodePort = reg.NextPort()
		}

		// Warn if overwriting.
		if existing := reg.Find(cwd, entry.WorkspaceID); existing != nil {
			log.Printf("Warning: overwriting existing entry for dir=%q workspace=%q", cwd, entry.WorkspaceID)
		}

		reg.Put(entry)
		if err := SaveRegistry(registryPath, reg); err != nil {
			log.Printf("Warning: failed to save registry: %v", err)
		} else {
			log.Printf("Registry saved to %s", registryPath)
		}
	} else {
		// --- Reconnect using saved credentials ---
		entries := reg.FindByDir(cwd)
		switch len(entries) {
		case 0:
			log.Fatal("No agent registered for this directory. Use --code to register.")
		case 1:
			entry = entries[0]
		default:
			if opts.WorkspaceID == "" {
				log.Fatalf("Multiple workspaces registered for this directory. Use --workspace to disambiguate.\nRegistered workspace IDs:")
			}
			entry = reg.Find(cwd, opts.WorkspaceID)
			if entry == nil {
				log.Fatalf("No entry found for workspace %q in this directory", opts.WorkspaceID)
			}
		}
		log.Printf("Using saved credentials (sandbox: %s)", entry.SandboxID)
	}

	// Determine opencode port: command-line override or entry value.
	opencodePort := entry.OpencodePort
	if opts.OpencodePortSet {
		opencodePort = opts.OpencodePort
	}

	// Auto-start opencode if requested.
	var opencodeProc *OpencodeProcess
	if opts.AutoStart {
		opencodeURL := fmt.Sprintf("http://localhost:%d", opencodePort)

		// Check if opencode is already listening.
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(opencodeURL + "/")
		if err == nil {
			resp.Body.Close()
			log.Printf("opencode already running on port %d, skipping auto-start", opencodePort)
		} else {
			log.Printf("Starting opencode on port %d...", opencodePort)
			opencodeProc, err = StartOpencode(opts.OpencodeBin, opencodePort, opts.OpencodeToken)
			if err != nil {
				log.Fatalf("Failed to start opencode: %v", err)
			}

			readyCtx, readyCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := opencodeProc.WaitReady(readyCtx, 30*time.Second); err != nil {
				readyCancel()
				opencodeProc.Stop()
				log.Fatalf("opencode failed to become ready: %v", err)
			}
			readyCancel()
		}

		// Use the auto-started URL unless --opencode-url was explicitly set.
		if !opts.OpencodeURLSet {
			opts.OpencodeURL = opencodeURL
		}
	}

	if opts.OpencodeURL == "" {
		opts.OpencodeURL = fmt.Sprintf("http://localhost:%d", opencodePort)
	}

	tunnelClient := NewClient(entry.Server, entry.SandboxID, entry.TunnelToken, opts.OpencodeURL, opts.OpencodeToken)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("Received %v, disconnecting...", sig)
		cancel()

		// Stop opencode subprocess if we started it.
		if opencodeProc != nil {
			opencodeProc.Stop()
		}
	}()

	log.Printf("Connecting to %s (forwarding to %s)...", entry.Server, opts.OpencodeURL)
	if err := tunnelClient.Run(ctx); err != nil && ctx.Err() == nil {
		if opencodeProc != nil {
			opencodeProc.Stop()
		}
		log.Fatalf("Agent error: %v", err)
	}

	// Clean up opencode on normal exit too.
	if opencodeProc != nil {
		opencodeProc.Stop()
	}
	log.Println("Agent disconnected.")
}
