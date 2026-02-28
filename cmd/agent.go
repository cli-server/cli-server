package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/imryao/cli-server/internal/agent"
	"github.com/spf13/cobra"
)

var (
	agentServer           string
	agentCode             string
	agentName             string
	agentOpencodeURL      string
	agentOpencodePassword string
	agentConfigPath       string
	agentAutoStart        bool
	agentOpencodeBin      string
	agentOpencodePort     int
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Local agent commands",
	Long:  `Commands for connecting a local opencode instance to cli-server via a WebSocket tunnel.`,
}

var agentConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect local opencode to cli-server",
	Long: `Establish a WebSocket tunnel between a local opencode instance and cli-server.

On first run, provide --server and --code to register with the server.
On subsequent runs, the saved credentials will be used automatically.

By default, opencode serve is started automatically on --opencode-port (4096).
Use --auto-start=false to disable this and manage opencode manually.`,
	Run: func(cmd *cobra.Command, args []string) {
		if agentConfigPath == "" {
			agentConfigPath = agent.DefaultConfigPath()
		}

		// Load existing config.
		cfg, err := agent.LoadConfig(agentConfigPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}

		// If no saved config, register with the code.
		if cfg == nil || cfg.SandboxID == "" {
			if agentServer == "" {
				log.Fatal("--server is required for first-time registration")
			}
			if agentCode == "" {
				log.Fatal("--code is required for first-time registration")
			}
			if agentName == "" {
				hostname, _ := os.Hostname()
				if hostname != "" {
					agentName = hostname
				} else {
					agentName = "Local Agent"
				}
			}

			log.Printf("Registering with server %s...", agentServer)
			cfg, err = agent.Register(agentServer, agentCode, agentName)
			if err != nil {
				log.Fatalf("Registration failed: %v", err)
			}
			log.Printf("Registered successfully (sandbox: %s)", cfg.SandboxID)

			if err := agent.SaveConfig(agentConfigPath, cfg); err != nil {
				log.Printf("Warning: failed to save config: %v", err)
			} else {
				log.Printf("Config saved to %s", agentConfigPath)
			}
		} else {
			log.Printf("Using saved credentials (sandbox: %s)", cfg.SandboxID)
			// Allow overriding server URL.
			if agentServer != "" {
				cfg.Server = agentServer
			}
		}

		// Auto-start opencode if requested.
		var opencodeProc *agent.OpencodeProcess
		if agentAutoStart {
			opencodeURL := fmt.Sprintf("http://localhost:%d", agentOpencodePort)

			// Check if opencode is already listening.
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(opencodeURL + "/")
			if err == nil {
				resp.Body.Close()
				log.Printf("opencode already running on port %d, skipping auto-start", agentOpencodePort)
			} else {
				log.Printf("Starting opencode on port %d...", agentOpencodePort)
				opencodeProc, err = agent.StartOpencode(agentOpencodeBin, agentOpencodePort)
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
			if !cmd.Flags().Changed("opencode-url") {
				agentOpencodeURL = opencodeURL
			}
		}

		if agentOpencodeURL == "" {
			agentOpencodeURL = fmt.Sprintf("http://localhost:%d", agentOpencodePort)
		}

		tunnelClient := agent.NewClient(cfg.Server, cfg.SandboxID, cfg.TunnelToken, agentOpencodeURL, agentOpencodePassword)

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

		log.Printf("Connecting to %s (forwarding to %s)...", cfg.Server, agentOpencodeURL)
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
	},
}

func init() {
	rootCmd.AddCommand(agentCmd)
	agentCmd.AddCommand(agentConnectCmd)

	agentConnectCmd.Flags().StringVar(&agentServer, "server", "", "CLI server URL (e.g., https://cli.example.com)")
	agentConnectCmd.Flags().StringVar(&agentCode, "code", "", "One-time registration code from Web UI")
	agentConnectCmd.Flags().StringVar(&agentName, "name", "", "Name for this agent (default: hostname)")
	agentConnectCmd.Flags().StringVar(&agentOpencodeURL, "opencode-url", "", "Local opencode server URL (default: http://localhost:{opencode-port})")
	agentConnectCmd.Flags().StringVar(&agentOpencodePassword, "opencode-password", "", "Local opencode server password")
	agentConnectCmd.Flags().StringVar(&agentConfigPath, "config", "", "Config file path (default: ~/.cli-server/agent.json)")
	agentConnectCmd.Flags().BoolVar(&agentAutoStart, "auto-start", true, "Automatically start opencode serve")
	agentConnectCmd.Flags().StringVar(&agentOpencodeBin, "opencode-bin", "opencode", "Path to the opencode binary")
	agentConnectCmd.Flags().IntVar(&agentOpencodePort, "opencode-port", 4096, "Port to start opencode on")
}
