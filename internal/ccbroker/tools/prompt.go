package tools

import (
	"fmt"
	"strings"
)

// ExecutorInfo describes one execution environment for the system prompt.
// Distinct from executorregistry.ExecutorInfo (which is a DB model);
// callers (e.g., runner/options.go) convert between them.
type ExecutorInfo struct {
	ExecutorID  string
	DisplayName string
	Type        string
	Tools       []string
	WorkingDir  string
	Description string
}

type PromptInput struct {
	ChannelType         string
	PreferredExecutorID string
	Executors           []ExecutorInfo
}

func BuildSystemPrompt(in PromptInput) string {
	var b strings.Builder
	if len(in.Executors) == 0 {
		b.WriteString("No execution environments are currently registered for this workspace.\n")
	} else {
		b.WriteString("You are operating in a workspace with the following execution environments:\n\n")
		for _, e := range in.Executors {
			marker := ""
			if e.ExecutorID == in.PreferredExecutorID {
				marker = " ★ PREFERRED FOR THIS SESSION"
			}
			fmt.Fprintf(&b, "- %s (id=%s, type=%s)%s\n", e.DisplayName, e.ExecutorID, e.Type, marker)
			if e.Description != "" {
				fmt.Fprintf(&b, "  %s\n", e.Description)
			}
			if e.WorkingDir != "" {
				fmt.Fprintf(&b, "  cwd: %s\n", e.WorkingDir)
			}
		}
	}

	if in.PreferredExecutorID != "" {
		fmt.Fprintf(&b, `
The user is operating from executor %q. Strongly prefer this executor for any
remote_* tool calls unless the task explicitly requires a different environment.
When unsure, ask before routing to a non-preferred executor.
`, in.PreferredExecutorID)
	}

	if in.ChannelType == "tui" {
		b.WriteString(`
The user is interacting through an interactive terminal client. You may use
AskUserQuestion freely for clarifications. The user can see tool calls in real
time and will be prompted to authorize potentially destructive operations.
`)
	} else {
		b.WriteString(`
The user is interacting through an instant messaging channel. Keep responses
concise and avoid asking too many clarifying questions in a row.
`)
	}
	return b.String()
}
