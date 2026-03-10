package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/agentserver/agentserver/internal/agent"
	"github.com/spf13/cobra"
)

var (
	agentServer        string
	agentCode          string
	agentName          string
	agentWorkspaceID   string
	agentOpencodeURL   string
	agentOpencodeToken string
	agentAutoStart     bool
	agentOpencodeBin   string
	agentOpencodePort  int
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
			Server:          agentServer,
			Code:            agentCode,
			Name:            agentName,
			WorkspaceID:     agentWorkspaceID,
			OpencodeURL:     agentOpencodeURL,
			OpencodeURLSet:  cmd.Flags().Changed("opencode-url"),
			OpencodeToken:   agentOpencodeToken,
			AutoStart:       agentAutoStart,
			OpencodeBin:     agentOpencodeBin,
			OpencodePort:    agentOpencodePort,
			OpencodePortSet: cmd.Flags().Changed("opencode-port"),
		})
	},
}

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered agents",
	Run: func(cmd *cobra.Command, args []string) {
		reg, err := agent.LoadRegistry(agent.DefaultRegistryPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if len(reg.Entries) == 0 {
			fmt.Println("No agents registered.")
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "DIRECTORY\tNAME\tWORKSPACE\tPORT\tSANDBOX")
		for _, e := range reg.Entries {
			dir := e.Dir
			if len(dir) > 40 {
				dir = "..." + dir[len(dir)-37:]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", dir, e.Name, e.WorkspaceID, e.OpencodePort, e.SandboxID)
		}
		w.Flush()
	},
}

var agentRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove an agent registration",
	Run: func(cmd *cobra.Command, args []string) {
		removeWorkspace, _ := cmd.Flags().GetString("workspace")
		removeDir, _ := cmd.Flags().GetString("dir")
		yes, _ := cmd.Flags().GetBool("yes")

		if removeDir == "" {
			var err error
			removeDir, err = os.Getwd()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: failed to get working directory: %v\n", err)
				os.Exit(1)
			}
		}

		reg, err := agent.LoadRegistry(agent.DefaultRegistryPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		entries := reg.FindByDir(removeDir)
		if len(entries) == 0 {
			fmt.Fprintf(os.Stderr, "No agents registered for directory: %s\n", removeDir)
			os.Exit(1)
		}

		var wsID string
		var entryName string
		switch {
		case removeWorkspace != "":
			wsID = removeWorkspace
			if e := reg.Find(removeDir, wsID); e != nil {
				entryName = e.Name
			}
		case len(entries) == 1:
			wsID = entries[0].WorkspaceID
			entryName = entries[0].Name
		default:
			fmt.Fprintf(os.Stderr, "Multiple agents registered for this directory. Use --workspace to specify which one:\n")
			for _, e := range entries {
				fmt.Fprintf(os.Stderr, "  workspace=%s  name=%s  sandbox=%s\n", e.WorkspaceID, e.Name, e.SandboxID)
			}
			os.Exit(1)
		}

		if !yes {
			label := entryName
			if label == "" {
				label = wsID
			}
			fmt.Printf("Remove agent %q for directory %s? [y/N] ", label, removeDir)
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer != "y" && answer != "yes" {
				fmt.Println("Aborted.")
				return
			}
		}

		if !reg.Remove(removeDir, wsID) {
			fmt.Fprintf(os.Stderr, "No entry found for dir=%q workspace=%q\n", removeDir, wsID)
			os.Exit(1)
		}

		if err := agent.SaveRegistry(agent.DefaultRegistryPath(), reg); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving registry: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Removed agent registration (dir=%s, workspace=%s)\n", removeDir, wsID)
	},
}

var agentVersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the agent version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("agentserver-agent %s\n", agent.Version)
	},
}

func init() {
	rootCmd.AddCommand(agentCmd)
	agentCmd.AddCommand(agentConnectCmd, agentListCmd, agentRemoveCmd, agentVersionCmd)

	agentConnectCmd.Flags().StringVar(&agentServer, "server", "", "Agent server URL (e.g., https://cli.example.com)")
	agentConnectCmd.Flags().StringVar(&agentCode, "code", "", "One-time registration code from Web UI")
	agentConnectCmd.Flags().StringVar(&agentName, "name", "", "Name for this agent (default: hostname)")
	agentConnectCmd.Flags().StringVar(&agentWorkspaceID, "workspace", "", "Workspace ID to connect to")
	agentConnectCmd.Flags().StringVar(&agentOpencodeURL, "opencode-url", "", "Local opencode server URL (default: http://localhost:{opencode-port})")
	agentConnectCmd.Flags().StringVar(&agentOpencodeToken, "opencode-token", "", "Local opencode server token")
	agentConnectCmd.Flags().BoolVar(&agentAutoStart, "auto-start", true, "Automatically start opencode serve")
	agentConnectCmd.Flags().StringVar(&agentOpencodeBin, "opencode-bin", "opencode", "Path to the opencode binary")
	agentConnectCmd.Flags().IntVar(&agentOpencodePort, "opencode-port", 4096, "Port to start opencode on")

	agentRemoveCmd.Flags().String("workspace", "", "Workspace ID of the agent to remove")
	agentRemoveCmd.Flags().String("dir", "", "Directory of the agent to remove (default: current directory)")
	agentRemoveCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}
