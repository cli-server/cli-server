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
	Server           string
	Code             string
	Name             string
	OpencodeURL      string
	OpencodeURLSet   bool // true if --opencode-url was explicitly provided
	OpencodeToken string
	ConfigPath       string
	AutoStart        bool
	OpencodeBin      string
	OpencodePort     int
}

// RunConnect executes the agent connect workflow.
func RunConnect(opts ConnectOptions) {
	if opts.ConfigPath == "" {
		opts.ConfigPath = DefaultConfigPath()
	}

	// Load existing config.
	cfg, err := LoadConfig(opts.ConfigPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// If no saved config, register with the code.
	if cfg == nil || cfg.SandboxID == "" {
		if opts.Server == "" {
			log.Fatal("--server is required for first-time registration")
		}
		if opts.Code == "" {
			log.Fatal("--code is required for first-time registration")
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
		cfg, err = Register(opts.Server, opts.Code, opts.Name)
		if err != nil {
			log.Fatalf("Registration failed: %v", err)
		}
		log.Printf("Registered successfully (sandbox: %s)", cfg.SandboxID)

		if err := SaveConfig(opts.ConfigPath, cfg); err != nil {
			log.Printf("Warning: failed to save config: %v", err)
		} else {
			log.Printf("Config saved to %s", opts.ConfigPath)
		}
	} else {
		log.Printf("Using saved credentials (sandbox: %s)", cfg.SandboxID)
		// Allow overriding server URL.
		if opts.Server != "" {
			cfg.Server = opts.Server
		}
	}

	// Auto-start opencode if requested.
	var opencodeProc *OpencodeProcess
	if opts.AutoStart {
		opencodeURL := fmt.Sprintf("http://localhost:%d", opts.OpencodePort)

		// Check if opencode is already listening.
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(opencodeURL + "/")
		if err == nil {
			resp.Body.Close()
			log.Printf("opencode already running on port %d, skipping auto-start", opts.OpencodePort)
		} else {
			log.Printf("Starting opencode on port %d...", opts.OpencodePort)
			opencodeProc, err = StartOpencode(opts.OpencodeBin, opts.OpencodePort)
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
		opts.OpencodeURL = fmt.Sprintf("http://localhost:%d", opts.OpencodePort)
	}

	tunnelClient := NewClient(cfg.Server, cfg.SandboxID, cfg.TunnelToken, opts.OpencodeURL, opts.OpencodeToken)

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

	log.Printf("Connecting to %s (forwarding to %s)...", cfg.Server, opts.OpencodeURL)
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
