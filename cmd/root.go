package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "cli-server",
	Short: "Self-hosted coding agent server",
	Long:  `cli-server provides a web-based interface to opencode, similar to code-server for VS Code.`,
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}
