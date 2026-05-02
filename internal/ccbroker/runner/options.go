package runner

import (
	"strconv"
	"strings"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// cliResumeID strips the cc-broker session-ID prefix to give the Claude CLI
// the bare UUID it expects from --resume. agentserver assigns session IDs
// like "cse_<UUID>" (per its own protocol); the CLI rejects anything that
// is not a valid UUID with:
//
//   Error: --resume requires a valid session ID or session title when used
//   with --print. Provided value "cse_..." is not a UUID and does not match
//   any session title.
func cliResumeID(sessionID string) string {
	return strings.TrimPrefix(sessionID, "cse_")
}

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
//
// Session lifecycle: the Claude CLI requires distinct flags for "create a
// new session with this UUID" (--session-id) versus "continue an existing
// session" (--resume). They are mutually exclusive: --session-id rejects
// IDs that already have a jsonl on disk, --resume rejects IDs that don't.
// runner.Run inspects ws.ClaudeDir before BuildSpec to decide.
type Spec struct {
	SessionUUID                     string // bare UUID (cse_ prefix already stripped)
	SessionExists                   bool   // true → --resume, false → --session-id
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
// Mirrors §2 of the design spec. sessionExists must be true iff a session
// jsonl for this UUID is already present in ws.ClaudeDir (caller checks via
// filepath.Glob before invoking).
func BuildSpec(ws *workspace.Workspace, sessionID string, cfg Config, sessionExists bool) Spec {
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
		SessionUUID:   cliResumeID(sessionID),
		SessionExists: sessionExists,
		Cwd:           ws.ProjectDir,
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
		agentsdk.WithCwd(s.Cwd),
		agentsdk.WithEnv(s.Env),
		agentsdk.WithSystemPrompt(s.SystemPrompt),
		agentsdk.WithAllowedTools(s.AllowedTools...),
	}
	if s.SessionUUID != "" {
		// --session-id rejects already-existing IDs; --resume rejects missing
		// ones. They are exclusive: pick exactly one based on whether the
		// session jsonl was present after Setup downloaded from OpenViking.
		if s.SessionExists {
			opts = append(opts, agentsdk.WithResume(s.SessionUUID))
		} else {
			opts = append(opts, agentsdk.WithSessionID(s.SessionUUID))
		}
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
