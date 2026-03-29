package main

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
	server        string
	code          string
	name          string
	workspaceID   string
	opencodeURL   string
	opencodeToken string
	autoStart     bool
	opencodeBin   string
	opencodePort  int

	// Claude Code specific flags.
	claudeBin     string
	claudeWorkDir string
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
			Server:          server,
			Code:            code,
			Name:            name,
			WorkspaceID:     workspaceID,
			OpencodeURL:     opencodeURL,
			OpencodeURLSet:  cmd.Flags().Changed("opencode-url"),
			OpencodeToken:   opencodeToken,
			AutoStart:       autoStart,
			OpencodeBin:     opencodeBin,
			OpencodePort:    opencodePort,
			OpencodePortSet: cmd.Flags().Changed("opencode-port"),
		})
	},
}

var listCmd = &cobra.Command{
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

var removeCmd = &cobra.Command{
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

var claudecodeCmd = &cobra.Command{
	Use:   "claudecode",
	Short: "Connect local Claude Code terminal to agentserver",
	Long: `Register a local Claude Code instance with agentserver and expose its terminal
via WebSocket tunnel. Users can access the terminal through the web browser at
claude-{id}.{domain}.

On first run, provide --server and --code to register with the server.
On subsequent runs, saved credentials are used automatically.`,
	Run: func(cmd *cobra.Command, args []string) {
		agent.RunClaudeCode(agent.ClaudeCodeOptions{
			Server:    server,
			Code:      code,
			Name:      name,
			ClaudeBin: claudeBin,
			WorkDir:   claudeWorkDir,
		})
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the agent version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("agentserver-agent %s\n", agent.Version)
	},
}

func init() {
	rootCmd.AddCommand(connectCmd, claudecodeCmd, listCmd, removeCmd, versionCmd)

	connectCmd.Flags().StringVar(&server, "server", "", "Agent server URL (e.g., https://cli.example.com)")
	connectCmd.Flags().StringVar(&code, "code", "", "One-time registration code from Web UI")
	connectCmd.Flags().StringVar(&name, "name", "", "Name for this agent (default: hostname)")
	connectCmd.Flags().StringVar(&workspaceID, "workspace", "", "Workspace ID to connect to")
	connectCmd.Flags().StringVar(&opencodeURL, "opencode-url", "", "Local opencode server URL (default: http://localhost:{opencode-port})")
	connectCmd.Flags().StringVar(&opencodeToken, "opencode-token", "", "Local opencode server token")
	connectCmd.Flags().BoolVar(&autoStart, "auto-start", true, "Automatically start opencode serve")
	connectCmd.Flags().StringVar(&opencodeBin, "opencode-bin", "opencode", "Path to the opencode binary")
	connectCmd.Flags().IntVar(&opencodePort, "opencode-port", 4096, "Port to start opencode on")

	claudecodeCmd.Flags().StringVar(&server, "server", "", "Agent server URL (e.g., https://cli.example.com)")
	claudecodeCmd.Flags().StringVar(&code, "code", "", "One-time registration code from Web UI")
	claudecodeCmd.Flags().StringVar(&name, "name", "", "Name for this agent (default: hostname)")
	claudecodeCmd.Flags().StringVar(&claudeBin, "claude-bin", "claude", "Path to the claude binary")
	claudecodeCmd.Flags().StringVar(&claudeWorkDir, "work-dir", "", "Working directory for Claude Code (default: current directory)")

	removeCmd.Flags().String("workspace", "", "Workspace ID of the agent to remove")
	removeCmd.Flags().String("dir", "", "Directory of the agent to remove (default: current directory)")
	removeCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
