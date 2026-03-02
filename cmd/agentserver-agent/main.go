package main

import (
	"os"

	"github.com/agentserver/agentserver/internal/agent"
	"github.com/spf13/cobra"
)

var (
	server           string
	code             string
	name             string
	opencodeURL      string
	opencodeToken string
	configPath       string
	autoStart        bool
	opencodeBin      string
	opencodePort     int
)

var rootCmd = &cobra.Command{
	Use:   "agentserver",
	Short: "Connect local opencode to agentserver",
	Long:  `Lightweight agent client that connects a local opencode instance to agentserver via a WebSocket tunnel.`,
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect local opencode to agentserver",
	Long: `Establish a WebSocket tunnel between a local opencode instance and agentserver.

On first run, provide --server and --code to register with the server.
On subsequent runs, the saved credentials will be used automatically.

By default, opencode serve is started automatically on --opencode-port (4096).
Use --auto-start=false to disable this and manage opencode manually.`,
	Run: func(cmd *cobra.Command, args []string) {
		agent.RunConnect(agent.ConnectOptions{
			Server:           server,
			Code:             code,
			Name:             name,
			OpencodeURL:      opencodeURL,
			OpencodeURLSet:   cmd.Flags().Changed("opencode-url"),
			OpencodeToken:    opencodeToken,
			ConfigPath:       configPath,
			AutoStart:        autoStart,
			OpencodeBin:      opencodeBin,
			OpencodePort:     opencodePort,
		})
	},
}

func init() {
	rootCmd.AddCommand(connectCmd)

	connectCmd.Flags().StringVar(&server, "server", "", "Agent server URL (e.g., https://cli.example.com)")
	connectCmd.Flags().StringVar(&code, "code", "", "One-time registration code from Web UI")
	connectCmd.Flags().StringVar(&name, "name", "", "Name for this agent (default: hostname)")
	connectCmd.Flags().StringVar(&opencodeURL, "opencode-url", "", "Local opencode server URL (default: http://localhost:{opencode-port})")
	connectCmd.Flags().StringVar(&opencodeToken, "opencode-token", "", "Local opencode server token")
	connectCmd.Flags().StringVar(&configPath, "config", "", "Config file path (default: ~/.agentserver/agent.json)")
	connectCmd.Flags().BoolVar(&autoStart, "auto-start", true, "Automatically start opencode serve")
	connectCmd.Flags().StringVar(&opencodeBin, "opencode-bin", "opencode", "Path to the opencode binary")
	connectCmd.Flags().IntVar(&opencodePort, "opencode-port", 4096, "Port to start opencode on")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
