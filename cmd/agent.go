package cmd

import (
	"github.com/agentserver/agentserver/internal/agent"
	"github.com/spf13/cobra"
)

var (
	agentServer           string
	agentCode             string
	agentName             string
	agentOpencodeURL      string
	agentOpencodeToken    string
	agentConfigPath       string
	agentAutoStart        bool
	agentOpencodeBin      string
	agentOpencodePort     int
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Local agent commands",
	Long:  `Commands for connecting a local opencode instance to agentserver via a WebSocket tunnel.`,
}

var agentConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect local opencode to agentserver",
	Long: `Establish a WebSocket tunnel between a local opencode instance and agentserver.

On first run, provide --server and --code to register with the server.
On subsequent runs, the saved credentials will be used automatically.

By default, opencode serve is started automatically on --opencode-port (4096).
Use --auto-start=false to disable this and manage opencode manually.`,
	Run: func(cmd *cobra.Command, args []string) {
		agent.RunConnect(agent.ConnectOptions{
			Server:           agentServer,
			Code:             agentCode,
			Name:             agentName,
			OpencodeURL:      agentOpencodeURL,
			OpencodeURLSet:   cmd.Flags().Changed("opencode-url"),
			OpencodeToken:    agentOpencodeToken,
			ConfigPath:       agentConfigPath,
			AutoStart:        agentAutoStart,
			OpencodeBin:      agentOpencodeBin,
			OpencodePort:     agentOpencodePort,
		})
	},
}

func init() {
	rootCmd.AddCommand(agentCmd)
	agentCmd.AddCommand(agentConnectCmd)

	agentConnectCmd.Flags().StringVar(&agentServer, "server", "", "Agent server URL (e.g., https://cli.example.com)")
	agentConnectCmd.Flags().StringVar(&agentCode, "code", "", "One-time registration code from Web UI")
	agentConnectCmd.Flags().StringVar(&agentName, "name", "", "Name for this agent (default: hostname)")
	agentConnectCmd.Flags().StringVar(&agentOpencodeURL, "opencode-url", "", "Local opencode server URL (default: http://localhost:{opencode-port})")
	agentConnectCmd.Flags().StringVar(&agentOpencodeToken, "opencode-token", "", "Local opencode server token")
	agentConnectCmd.Flags().StringVar(&agentConfigPath, "config", "", "Config file path (default: ~/.agentserver/agent.json)")
	agentConnectCmd.Flags().BoolVar(&agentAutoStart, "auto-start", true, "Automatically start opencode serve")
	agentConnectCmd.Flags().StringVar(&agentOpencodeBin, "opencode-bin", "opencode", "Path to the opencode binary")
	agentConnectCmd.Flags().IntVar(&agentOpencodePort, "opencode-port", 4096, "Port to start opencode on")
}
