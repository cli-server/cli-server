package runner

import (
	"strconv"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// Config holds the broker-level configuration relevant to spawning a CC worker.
// All fields are populated from cc-broker's process env at startup.
type Config struct {
	SystemPrompt             string
	MaxTurns                 int
	AnthropicAPIKey          string
	AnthropicAuthToken       string
	AnthropicBaseURL         string
	DisableFileCheckpointing bool
	AutoCompactWindow        int
}

// Spec is the SDK-agnostic projection of "everything we are about to pass to
// the Claude SDK for one turn." It exists so tests can assert exactly what we
// would have asked the SDK to do, without depending on the SDK's unexported
// queryConfig. ToOptions() translates a Spec into an agentsdk option slice.
type Spec struct {
	Resume                          string
	Cwd                             string
	Env                             map[string]string
	SystemPrompt                    string
	AllowedTools                    []string
	DisallowedTools                 []string
	PermissionBypass                bool
	AllowDangerouslySkipPermissions bool
	MaxTurns                        int
	McpServer                       *agentsdk.McpSdkServer
}

// BuildSpec composes a Spec from workspace + sessionID + config. Pure.
// Mirrors §2 of the design spec.
func BuildSpec(ws *workspace.Workspace, sessionID string, cfg Config) Spec {
	env := map[string]string{
		"CLAUDE_CONFIG_DIR":                  ws.ClaudeDir,
		"CLAUDE_COWORK_MEMORY_PATH_OVERRIDE": ws.MemoryDir,
	}
	if cfg.AnthropicAPIKey != "" {
		env["ANTHROPIC_API_KEY"] = cfg.AnthropicAPIKey
	}
	if cfg.AnthropicAuthToken != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = cfg.AnthropicAuthToken
	}
	if cfg.AnthropicBaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = cfg.AnthropicBaseURL
	}
	if cfg.DisableFileCheckpointing {
		env["CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING"] = "1"
	}
	if cfg.AutoCompactWindow > 0 {
		env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] = strconv.Itoa(cfg.AutoCompactWindow)
	}
	return Spec{
		Resume:       sessionID,
		Cwd:          ws.ProjectDir,
		Env:          env,
		SystemPrompt: cfg.SystemPrompt,
		AllowedTools: []string{"WebSearch", "WebFetch", "mcp__cc-broker__*"},
		DisallowedTools: []string{
			"Bash", "Read", "Edit", "Write", "Glob", "Grep", "LS",
			"Task", "BashOutput", "KillShell", "NotebookEdit",
		},
		PermissionBypass:                true,
		AllowDangerouslySkipPermissions: true,
		MaxTurns:                        cfg.MaxTurns,
	}
}

// ToOptions translates a Spec into the agentsdk option slice that
// NewClient/Query consume. The McpServer field is wired separately because
// callers usually build the MCP tools after BuildSpec.
func (s Spec) ToOptions() []agentsdk.QueryOption {
	opts := []agentsdk.QueryOption{
		agentsdk.WithResume(s.Resume),
		agentsdk.WithCwd(s.Cwd),
		agentsdk.WithEnv(s.Env),
		agentsdk.WithSystemPrompt(s.SystemPrompt),
		agentsdk.WithAllowedTools(s.AllowedTools...),
	}
	if len(s.DisallowedTools) > 0 {
		opts = append(opts, agentsdk.WithDisallowedTools(s.DisallowedTools...))
	}
	if s.PermissionBypass {
		opts = append(opts, agentsdk.WithPermissionMode(agentsdk.PermissionBypassAll))
	}
	if s.AllowDangerouslySkipPermissions {
		opts = append(opts, agentsdk.WithAllowDangerouslySkipPermissions())
	}
	if s.MaxTurns > 0 {
		opts = append(opts, agentsdk.WithMaxTurns(s.MaxTurns))
	}
	if s.McpServer != nil {
		opts = append(opts, agentsdk.WithMcpServers(map[string]agentsdk.McpServerConfig{
			"cc-broker": {SDK: s.McpServer},
		}))
	}
	return opts
}
