package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// byName is a small helper used by tools tests to pick a registered tool out
// of a slice without writing the same find-by-name loop in every test.
func byName(tools []agentsdk.McpTool, name string) agentsdk.McpTool {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	panic("tool not found: " + name)
}

func TestWorkspaceTools_WriteReadLS(t *testing.T) {
	dir := t.TempDir()
	tctx := &Context{Workspace: &workspace.Workspace{ClaudeDir: dir}}
	tools := workspaceTools(tctx)

	w := byName(tools, "workspace_write")
	if r, _ := w.Handler(context.Background(),
		json.RawMessage(`{"path":"skills/foo.md","content":"hello"}`)); r.IsError {
		t.Fatalf("write IsError: %v", r.Content)
	}

	r := byName(tools, "workspace_read")
	rd, _ := r.Handler(context.Background(), json.RawMessage(`{"path":"skills/foo.md"}`))
	if rd.Content[0].Text != "hello" {
		t.Errorf("read got %q want hello", rd.Content[0].Text)
	}

	l := byName(tools, "workspace_ls")
	lr, _ := l.Handler(context.Background(), json.RawMessage(`{"path":"skills"}`))
	if !strings.Contains(lr.Content[0].Text, "foo.md") {
		t.Errorf("ls missing foo.md: %q", lr.Content[0].Text)
	}
}

func TestWorkspaceTools_PathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	tctx := &Context{Workspace: &workspace.Workspace{ClaudeDir: dir}}
	w := byName(workspaceTools(tctx), "workspace_write")
	r, _ := w.Handler(context.Background(),
		json.RawMessage(`{"path":"../escaped","content":"x"}`))
	if !r.IsError {
		t.Errorf("expected IsError for path traversal")
	}
}
