package cmd

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/imryao/cli-server/internal/agent"
	"github.com/spf13/cobra"
)

var (
	agentServer          string
	agentCode            string
	agentName            string
	agentOpencodeURL     string
	agentOpencodePassword string
	agentConfigPath      string
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
On subsequent runs, the saved credentials will be used automatically.`,
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

		if agentOpencodeURL == "" {
			agentOpencodeURL = "http://localhost:4096"
		}

		client := agent.NewClient(cfg.Server, cfg.SandboxID, cfg.TunnelToken, agentOpencodeURL, agentOpencodePassword)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Handle graceful shutdown.
		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			sig := <-sigCh
			log.Printf("Received %v, disconnecting...", sig)
			cancel()
		}()

		log.Printf("Connecting to %s (forwarding to %s)...", cfg.Server, agentOpencodeURL)
		if err := client.Run(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("Agent error: %v", err)
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
	agentConnectCmd.Flags().StringVar(&agentOpencodeURL, "opencode-url", "http://localhost:4096", "Local opencode server URL")
	agentConnectCmd.Flags().StringVar(&agentOpencodePassword, "opencode-password", "", "Local opencode server password")
	agentConnectCmd.Flags().StringVar(&agentConfigPath, "config", "", "Config file path (default: ~/.cli-server/agent.json)")
}
