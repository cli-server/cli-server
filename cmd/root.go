package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "cli-server",
	Short: "Cloud-hosted Claude Code terminal server",
	Long:  `cli-server provides a web-based terminal interface to Claude Code CLI, similar to code-server.`,
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}
