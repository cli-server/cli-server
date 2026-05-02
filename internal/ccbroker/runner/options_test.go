package runner

import (
	"testing"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

func TestBuildSpec(t *testing.T) {
	ws := &workspace.Workspace{
		WorkspaceID: "ws1",
		SessionID:   "cse_abc",
		ClaudeDir:   "/tmp/x/claude-config",
		ProjectDir:  "/tmp/x/project",
		MemoryDir:   "/tmp/x/claude-config/projects/ws_ws1/memory",
	}
	cfg := Config{
		SystemPrompt:             "you are a helpful assistant",
		MaxTurns:                 50,
		AnthropicAPIKey:          "",
		AnthropicAuthToken:       "tok-123",
		AnthropicBaseURL:         "https://gateway.example",
		DisableFileCheckpointing: true,
		AutoCompactWindow:        165000,
	}
	spec := BuildSpec(ws, "cse_abc", cfg)

	if spec.Resume != "abc" {
		t.Errorf("Resume=%q want abc (cse_ prefix stripped for CLI compatibility)", spec.Resume)
	}
	if spec.Cwd != ws.ProjectDir {
		t.Errorf("Cwd=%q want %s", spec.Cwd, ws.ProjectDir)
	}
	wantEnv := map[string]string{
		"CLAUDE_CONFIG_DIR":                      ws.ClaudeDir,
		"CLAUDE_COWORK_MEMORY_PATH_OVERRIDE":     ws.MemoryDir,
		"ANTHROPIC_AUTH_TOKEN":                   "tok-123",
		"ANTHROPIC_BASE_URL":                     "https://gateway.example",
		"CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING": "1",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW":        "165000",
	}
	for k, v := range wantEnv {
		if spec.Env[k] != v {
			t.Errorf("Env[%q]=%q want %q", k, spec.Env[k], v)
		}
	}
	if _, ok := spec.Env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY should be omitted when empty")
	}

	wantTools := []string{"WebSearch", "WebFetch", "mcp__cc-broker__*"}
	if len(spec.AllowedTools) != len(wantTools) {
		t.Fatalf("AllowedTools=%v want %v", spec.AllowedTools, wantTools)
	}
	for i, w := range wantTools {
		if spec.AllowedTools[i] != w {
			t.Errorf("AllowedTools[%d]=%q want %q", i, spec.AllowedTools[i], w)
		}
	}
	wantDisallowed := []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "LS", "Task", "BashOutput", "KillShell", "NotebookEdit"}
	if len(spec.DisallowedTools) != len(wantDisallowed) {
		t.Fatalf("DisallowedTools=%v want %v", spec.DisallowedTools, wantDisallowed)
	}
	for i, w := range wantDisallowed {
		if spec.DisallowedTools[i] != w {
			t.Errorf("DisallowedTools[%d]=%q want %q", i, spec.DisallowedTools[i], w)
		}
	}
	if !spec.PermissionBypass {
		t.Errorf("PermissionBypass must be true")
	}
	if !spec.AllowDangerouslySkipPermissions {
		t.Errorf("AllowDangerouslySkipPermissions must be true (paired with PermissionBypass per SDK)")
	}
	if spec.MaxTurns != 50 {
		t.Errorf("MaxTurns=%d want 50", spec.MaxTurns)
	}
	if spec.SystemPrompt != "you are a helpful assistant" {
		t.Errorf("SystemPrompt mismatch")
	}
}

func TestBuildSpec_PrefersAPIKeyWhenBothSet(t *testing.T) {
	ws := &workspace.Workspace{ClaudeDir: "/c", ProjectDir: "/p", MemoryDir: "/m"}
	cfg := Config{
		AnthropicAPIKey:    "key-1",
		AnthropicAuthToken: "tok-2",
	}
	spec := BuildSpec(ws, "sid", cfg)

	if spec.Env["ANTHROPIC_API_KEY"] != "key-1" {
		t.Errorf("API_KEY not forwarded")
	}
	if spec.Env["ANTHROPIC_AUTH_TOKEN"] != "tok-2" {
		t.Errorf("AUTH_TOKEN should still be forwarded so CLI picks whichever it prefers")
	}
}
