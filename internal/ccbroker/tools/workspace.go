package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
)

type workspaceReadInput struct {
	Path string `json:"path"`
}
type workspaceWriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type workspaceLSInput struct {
	Path string `json:"path,omitempty"`
}

func workspaceTools(tctx *Context) []agentsdk.McpTool {
	return []agentsdk.McpTool{
		agentsdk.Tool[workspaceReadInput]("workspace_read",
			"Read a file from the workspace context (skills, instructions, memory).",
			func(ctx context.Context, in workspaceReadInput) (*agentsdk.McpToolResult, error) {
				p, err := safeWorkspacePath(tctx, in.Path)
				if err != nil {
					return errResult(err), nil
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return errResult(err), nil
				}
				return textResult(string(data)), nil
			}),
		agentsdk.Tool[workspaceWriteInput]("workspace_write",
			"Write a file to the workspace context. Persists across sessions via OpenViking.",
			func(ctx context.Context, in workspaceWriteInput) (*agentsdk.McpToolResult, error) {
				p, err := safeWorkspacePath(tctx, in.Path)
				if err != nil {
					return errResult(err), nil
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return errResult(err), nil
				}
				if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
					return errResult(err), nil
				}
				return textResult("Written successfully"), nil
			}),
		agentsdk.Tool[workspaceLSInput]("workspace_ls",
			"List files in a workspace context directory.",
			func(ctx context.Context, in workspaceLSInput) (*agentsdk.McpToolResult, error) {
				p, err := safeWorkspacePath(tctx, in.Path)
				if err != nil {
					return errResult(err), nil
				}
				entries, err := os.ReadDir(p)
				if err != nil {
					return errResult(err), nil
				}
				var names []string
				for _, e := range entries {
					name := e.Name()
					if e.IsDir() {
						name += "/"
					}
					names = append(names, name)
				}
				return textResult(strings.Join(names, "\n")), nil
			}),
	}
}

// safeWorkspacePath joins ClaudeDir with rel and ensures the result stays
// inside ClaudeDir (no path traversal).
func safeWorkspacePath(tctx *Context, rel string) (string, error) {
	if tctx.Workspace == nil {
		return "", fmt.Errorf("workspace not initialised")
	}
	abs := filepath.Join(tctx.Workspace.ClaudeDir, filepath.Clean(rel))
	// Require abs to be exactly ClaudeDir or a descendant of it.
	// Use a trailing-slash anchor to prevent /tmp/foo matching /tmp/foobar.
	base := tctx.Workspace.ClaudeDir
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %s", rel)
	}
	return abs, nil
}
