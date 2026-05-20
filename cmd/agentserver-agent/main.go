// Package main is the entrypoint for the bundled CLI shipped inside
// nanoclaw and claudecode sandbox images. The full stateless-cc agent
// (claudecode driver, executor mode, TUI, etc.) was removed when codex
// became the sole agent stack; only the `mcp-server` subcommand
// survives because nanoclaw spawns it to expose Claude-side MCP tools.
package main

import (
	"fmt"
	"os"

	"github.com/agentserver/agentserver/internal/agent"
	"github.com/agentserver/agentserver/internal/mcpbridge"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "agentserver",
	Short: "Minimal CLI shipped inside agentserver sandbox images",
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
		fmt.Printf("agentserver %s\n", agent.Version)
	},
}

func main() {
	rootCmd.AddCommand(mcpServerCmd, versionCmd)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
