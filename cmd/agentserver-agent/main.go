package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/agentserver/agentserver/internal/agent"
	"github.com/agentserver/agentserver/internal/mcpbridge"
	"github.com/spf13/cobra"
)

var (
	server        string
	name          string
	opencodeURL   string
	opencodeToken string
	autoStart     bool
	opencodeBin   string
	opencodePort  int

	// Claude Code specific flags.
	claudeBin     string
	claudeWorkDir string

	// Session flags.
	skipOpenBrowser bool
	resumeID       string
	continueFlag   bool
)

var rootCmd = &cobra.Command{
	Use:   "agentserver",
	Short: "Connect local Claude Code agent to agentserver",
	Long: `Authenticate and connect a local Claude Code instance to agentserver.

On first run, authenticates via OAuth Device Flow and registers a new agent.
On subsequent runs, reuses saved credentials and creates a new session.

Use --resume <sandbox-id> to reconnect to a previous session,
or -c/--continue to resume the most recent one.`,
	Run: func(cmd *cobra.Command, args []string) {
		agent.RunClaudeCode(agent.ClaudeCodeOptions{
			Server:          server,
			Name:            name,
			SkipOpenBrowser: skipOpenBrowser,
			ClaudeBin:       claudeBin,
			WorkDir:         claudeWorkDir,
			Resume:          resumeID,
			Continue:        continueFlag,
		})
	},
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect local opencode to agentserver",
	Long: `Establish a WebSocket tunnel between a local opencode instance and agentserver.

On first run, authenticates via OAuth and registers a new agent.
On subsequent runs, reuses saved credentials and creates a new session.

By default, opencode serve is started automatically on --opencode-port (4096).
Use --auto-start=false to disable this and manage opencode manually.`,
	Run: func(cmd *cobra.Command, args []string) {
		agent.RunConnect(agent.ConnectOptions{
			Server:          server,
			Name:            name,
			SkipOpenBrowser: skipOpenBrowser,
			Resume:          resumeID,
			Continue:        continueFlag,
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
	Short: "List all sessions",
	Run: func(cmd *cobra.Command, args []string) {
		sessions, err := agent.ListSessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		if len(sessions) == 0 {
			fmt.Println("No sessions.")
			return
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "SANDBOX\tNAME\tTYPE\tWORKSPACE\tDIRECTORY\tACTIVE")
		for _, s := range sessions {
			dir := s.Dir
			if len(dir) > 35 {
				dir = "..." + dir[len(dir)-32:]
			}
			active := ""
			if agent.IsSessionActive(s) {
				active = fmt.Sprintf("PID %d", s.PID)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				truncID(s.SandboxID, 8), s.Name, s.Type, truncID(s.WorkspaceID, 8), dir, active)
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
via WebSocket tunnel.

On first run, authenticates via OAuth and registers a new agent.
On subsequent runs, reuses saved credentials and creates a new session.`,
	Run: func(cmd *cobra.Command, args []string) {
		agent.RunClaudeCode(agent.ClaudeCodeOptions{
			Server:          server,
			Name:            name,
			SkipOpenBrowser: skipOpenBrowser,
			ClaudeBin:       claudeBin,
			WorkDir:         claudeWorkDir,
			Resume:          resumeID,
			Continue:        continueFlag,
		})
	},
}

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with agentserver via OAuth Device Flow",
	Long: `Authenticate with agentserver using OAuth Device Flow.

Opens a browser for authentication. If browser is unavailable, displays a URL
and QR code for manual login. Saves credentials for future use.`,
	Run: func(cmd *cobra.Command, args []string) {
		if err := agent.RunLogin(agent.LoginOptions{
			ServerURL:       server,
			SkipOpenBrowser: skipOpenBrowser,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	},
}

var taskWorkerCmd = &cobra.Command{
	Use:   "task-worker",
	Short: "Run as a task worker that polls and executes delegated tasks",
	Run: func(cmd *cobra.Command, args []string) {
		taskServer, _ := cmd.Flags().GetString("server")
		taskProxyToken, _ := cmd.Flags().GetString("proxy-token")
		taskSandboxID, _ := cmd.Flags().GetString("sandbox-id")
		taskWorkDir, _ := cmd.Flags().GetString("work-dir")
		taskClaudeBin, _ := cmd.Flags().GetString("claude-bin")

		if taskServer == "" || taskProxyToken == "" || taskSandboxID == "" {
			reg, err := agent.LoadRegistry(agent.DefaultRegistryPath())
			if err == nil && len(reg.Entries) > 0 {
				entry := reg.Entries[0]
				if taskServer == "" {
					taskServer = entry.Server
				}
				if taskSandboxID == "" {
					taskSandboxID = entry.SandboxID
				}
				if taskProxyToken == "" {
					taskProxyToken = entry.TunnelToken
				}
			}
		}

		if taskServer == "" || taskSandboxID == "" {
			fmt.Fprintln(os.Stderr, "Error: --server and --sandbox-id are required (or a valid registry entry)")
			os.Exit(1)
		}

		if taskWorkDir == "" {
			taskWorkDir, _ = os.Getwd()
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			<-sigCh
			cancel()
		}()
		agent.RunTaskWorker(ctx, agent.TaskWorkerOptions{
			ServerURL:  taskServer,
			ProxyToken: taskProxyToken,
			SandboxID:  taskSandboxID,
			Workdir:    taskWorkDir,
			CLIPath:    taskClaudeBin,
		})
	},
}

var mcpServerCmd = &cobra.Command{
	Use:   "mcp-server",
	Short: "Run as an MCP stdio server for Claude Code integration",
	Run: func(cmd *cobra.Command, args []string) {
		mcpbridge.RunMCPServer()
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
	rootCmd.AddCommand(connectCmd, claudecodeCmd, loginCmd, listCmd, removeCmd, taskWorkerCmd, mcpServerCmd, versionCmd)

	// Root command flags (default: Claude Code agent).
	rootCmd.Flags().StringVar(&server, "server", "https://agent.cs.ac.cn", "Agent server URL")
	rootCmd.Flags().StringVar(&name, "name", "", "Name for this agent (default: hostname)")
	rootCmd.Flags().BoolVar(&skipOpenBrowser, "skip-open-browser", false, "Don't auto-open browser, show URL + QR only")
	rootCmd.Flags().StringVar(&claudeBin, "claude-bin", "claude", "Path to the claude binary")
	rootCmd.Flags().StringVar(&claudeWorkDir, "work-dir", "", "Working directory (default: current directory)")
	rootCmd.Flags().StringVarP(&resumeID, "resume", "r", "", "Resume a previous session by sandbox ID")
	rootCmd.Flags().BoolVarP(&continueFlag, "continue", "c", false, "Resume the most recent session")

	// Login command flags.
	loginCmd.Flags().StringVar(&server, "server", "https://agent.cs.ac.cn", "Agent server URL")
	loginCmd.Flags().BoolVar(&skipOpenBrowser, "skip-open-browser", false, "Don't auto-open browser, show URL + QR only")

	// Connect command flags.
	connectCmd.Flags().StringVar(&server, "server", "", "Agent server URL")
	connectCmd.Flags().StringVar(&name, "name", "", "Name for this agent (default: hostname)")
	connectCmd.Flags().BoolVar(&skipOpenBrowser, "skip-open-browser", false, "Don't auto-open browser, show URL + QR only")
	connectCmd.Flags().StringVarP(&resumeID, "resume", "r", "", "Resume a previous session by sandbox ID")
	connectCmd.Flags().BoolVarP(&continueFlag, "continue", "c", false, "Resume the most recent session")
	connectCmd.Flags().StringVar(&opencodeURL, "opencode-url", "", "Local opencode server URL")
	connectCmd.Flags().StringVar(&opencodeToken, "opencode-token", "", "Local opencode server token")
	connectCmd.Flags().BoolVar(&autoStart, "auto-start", true, "Automatically start opencode serve")
	connectCmd.Flags().StringVar(&opencodeBin, "opencode-bin", "opencode", "Path to the opencode binary")
	connectCmd.Flags().IntVar(&opencodePort, "opencode-port", 4096, "Port to start opencode on")

	// Claudecode command flags.
	claudecodeCmd.Flags().StringVar(&server, "server", "", "Agent server URL")
	claudecodeCmd.Flags().StringVar(&name, "name", "", "Name for this agent (default: hostname)")
	claudecodeCmd.Flags().BoolVar(&skipOpenBrowser, "skip-open-browser", false, "Don't auto-open browser, show URL + QR only")
	claudecodeCmd.Flags().StringVar(&claudeBin, "claude-bin", "claude", "Path to the claude binary")
	claudecodeCmd.Flags().StringVar(&claudeWorkDir, "work-dir", "", "Working directory (default: current directory)")
	claudecodeCmd.Flags().StringVarP(&resumeID, "resume", "r", "", "Resume a previous session by sandbox ID")
	claudecodeCmd.Flags().BoolVarP(&continueFlag, "continue", "c", false, "Resume the most recent session")

	// Task worker flags.
	taskWorkerCmd.Flags().String("server", "", "Agent server URL")
	taskWorkerCmd.Flags().String("proxy-token", "", "Sandbox proxy token")
	taskWorkerCmd.Flags().String("sandbox-id", "", "Sandbox ID")
	taskWorkerCmd.Flags().String("work-dir", "", "Working directory for task execution")
	taskWorkerCmd.Flags().String("claude-bin", "claude", "Path to the claude binary")

	// Remove command flags.
	removeCmd.Flags().String("workspace", "", "Workspace ID of the agent to remove")
	removeCmd.Flags().String("dir", "", "Directory of the agent to remove")
	removeCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}

func truncID(id string, n int) string {
	if len(id) < n {
		return id
	}
	return id[:n]
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
