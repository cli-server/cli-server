# Multi-Agent Interaction Implementation Design

**Date**: 2026-03-30
**Status**: Draft
**Scope**: End-to-end implementation design for Claude Code agent discovery, task delegation, and inter-agent collaboration
**Prerequisite**: [Multi-Agent Discovery, Capability Awareness & Interaction Spec](2026-03-25-multi-agent-discovery-interaction-design.md)

## Scope & Constraints

**First iteration focuses exclusively on Claude Code agents.** Other agent types (opencode, openclaw, nanoclaw) are not supported in this version. The data model retains the `agent_type` field for future extensibility, but all runtime logic targets `claudecode` only.

**Key decisions:**

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Agent type scope | `claudecode` only | Simplify implementation; Claude Code is the primary agent |
| Task execution engine | Go Claude Agent SDK | Wraps `claude` CLI binary; aligns with official Python/TS SDKs |
| SDK repository | Separate repo (`claude-agent-sdk-go`) | Independent module; reusable outside agentserver |
| Phase 1 worktree | Discard, restart from main | Stale worktree with outdated migration numbers |

---

## Phase Structure

```
Phase 1: Go Claude Agent SDK ─────────────────────────────
  │  New Go module: github.com/agentserver/claude-agent-sdk-go
  │  Wraps claude CLI process management + stdin/stdout JSON protocol
  │  Full type system mapping Options/Messages
  │  query() streaming + session management
  │
Phase 2: Foundation (Agent Cards + Discovery) ────────────
  │  DB: agent_cards table (migration 013)
  │  API: AgentAuthMiddleware, card registration, discovery
  │  Background: AgentHealthMonitor
  │  Tunnel: extended heartbeat, auto-register card
  │
Phase 3: Task Delegation ─────────────────────────────────
  │  DB: agent_tasks table (migration 014)
  │  API: task create/get/list
  │  Task delivery: tunnel frames (local) + HTTP (cloud)
  │  Task executor: uses Go Agent SDK
  │  Task cleanup worker
  │
Phase 4: MCP Integration + Tool Injection ────────────────
  │  agentserver-mcp-bridge Go binary
  │  inject_tools tunnel frame (local agent)
  │  Server-side MCP proxy (agent/{id}/tool routing)
  │  discover_agents, delegate_task, check_task MCP tools
  │
Phase 5: Security + Observability ────────────────────────
   JWT fast-path for cloud-to-cloud direct calls
   Audit logging (agent_interactions table)
   Workspace delegation mode (auto/approval)
   Dashboard API for agent/task visibility
```

**Delivery milestones:**
- Phase 1 complete: Go Agent SDK usable independently
- Phase 2 complete: agents can register capabilities and discover each other
- Phase 3 complete: agents can delegate tasks to peers and receive results
- Phase 4 complete: AI model autonomously discovers and delegates via MCP tools
- Phase 5 complete: production-grade security, audit trail, operator visibility

---

## Phase 1: Go Claude Agent SDK

### Architecture

The Agent SDK wraps the `claude` CLI binary, communicating via a bidirectional JSON protocol over stdin/stdout. This is the same architecture as the official Python and TypeScript SDKs.

```
┌──────────────────────────────────────────────────┐
│  User Application (Go)                           │
│                                                  │
│  stream := agentsdk.Query(ctx, prompt, opts...)  │
│  for stream.Next() {                             │
│      msg := stream.Current()                     │
│  }                                               │
└──────────┬───────────────────────────────────────┘
           │ Go API
┌──────────▼───────────────────────────────────────┐
│  claude-agent-sdk-go                             │
│                                                  │
│  ┌─────────────┐  ┌──────────────┐              │
│  │ Query/Client │  │ Control      │              │
│  │ (public API) │  │ Protocol     │              │
│  │              │  │ (hooks, MCP, │              │
│  │              │  │  permissions)│              │
│  └──────┬───────┘  └──────┬───────┘              │
│         │                  │                      │
│  ┌──────▼──────────────────▼───────┐             │
│  │  Transport (SubprocessTransport)│             │
│  │  stdin writer / stdout reader   │             │
│  └──────┬──────────────────┬───────┘             │
└─────────│──────────────────│─────────────────────┘
          │ stdin (JSON)     │ stdout (stream-json)
┌─────────▼──────────────────▼───────────────────┐
│  claude CLI binary                              │
│  (spawned as subprocess)                        │
└─────────────────────────────────────────────────┘
```

### Repository Structure

Repository: `https://github.com/agentserver/claude-agent-sdk-go.git`

Project organization follows `anthropic-sdk-go` conventions:

```
claude-agent-sdk-go/
├── go.mod                      // module github.com/agentserver/claude-agent-sdk-go
├── sdk.go                      // Query() entry point, Stream type
├── client.go                   // Client (interactive bidirectional sessions)
├── options.go                  // QueryOption functional options, queryConfig
├── message.go                  // SDKMessage union + all message subtypes
├── content.go                  // ContentBlock, TextBlock, ToolUseBlock, etc.
├── hook.go                     // HookEvent, HookMatcher, HookCallback
├── mcp.go                      // McpServerConfig variants, tool(), McpSdkServer
├── session.go                  // ListSessions, GetSessionMessages, mutations
├── error.go                    // CLINotFoundError, ProcessError, etc.
├── internal/
│   ├── transport/
│   │   ├── transport.go        // Transport interface
│   │   └── subprocess.go       // SubprocessTransport implementation
│   ├── protocol/
│   │   ├── control.go          // Control protocol message types
│   │   └── query.go            // Query internal: message routing, hook dispatch
│   ├── clilookup.go            // Claude CLI binary discovery
│   └── version.go              // SDK version constant
├── examples/
│   ├── basic/main.go           // Minimal query example
│   ├── hooks/main.go           // Hook callback example
│   ├── mcp/main.go             // MCP server example
│   └── interactive/main.go     // Client interactive example
└── sdk_test.go                 // Integration tests
```

### Public API

#### One-shot query

```go
// Query creates a new Claude Code session and streams messages.
// It spawns a claude subprocess, sends the prompt, and returns
// a Stream that yields messages as they arrive.
func Query(ctx context.Context, prompt string, opts ...QueryOption) *Stream

// Stream iterates over SDK messages from the claude process.
// Follows the same pattern as anthropic-sdk-go's ssestream.Stream.
type Stream struct { /* ... */ }

func (s *Stream) Next() bool              // Advance to next message; false on EOF or error
func (s *Stream) Current() SDKMessage     // Current message (valid after Next() returns true)
func (s *Stream) Err() error              // Error that stopped iteration (nil on clean finish)
func (s *Stream) Close() error            // Close subprocess and release resources
func (s *Stream) Interrupt() error        // Send interrupt signal to claude process
func (s *Stream) Result() (*ResultMessage, error)  // Drain stream and return final result
```

#### Interactive client

```go
// Client maintains a persistent connection to a claude subprocess
// for multi-turn conversations.
type Client struct { /* ... */ }

func NewClient(opts ...QueryOption) *Client
func (c *Client) Connect(ctx context.Context) error
func (c *Client) Send(ctx context.Context, prompt string) error
func (c *Client) Messages() <-chan SDKMessage
func (c *Client) Interrupt() error
func (c *Client) SetModel(model string) error
func (c *Client) SetPermissionMode(mode PermissionMode) error
func (c *Client) McpStatus() ([]McpServerStatus, error)
func (c *Client) StopTask(taskID string) error
func (c *Client) Close() error
```

#### Session management

```go
func ListSessions(opts *ListSessionsOptions) ([]SessionInfo, error)
func GetSessionInfo(sessionID string, opts *GetSessionInfoOptions) (*SessionInfo, error)
func GetSessionMessages(sessionID string, opts *GetSessionMessagesOptions) ([]SessionMessage, error)
func RenameSession(sessionID, title string, opts *SessionMutationOptions) error
func TagSession(sessionID string, tag *string, opts *SessionMutationOptions) error
func DeleteSession(sessionID string, opts *SessionMutationOptions) error
```

### Options (functional options pattern)

```go
type QueryOption func(*queryConfig)

// Model & Reasoning
func WithModel(model string) QueryOption
func WithFallbackModel(model string) QueryOption
func WithThinking(config ThinkingConfig) QueryOption
func WithEffort(effort Effort) QueryOption

// Tools & Permissions
func WithAllowedTools(tools ...string) QueryOption
func WithDisallowedTools(tools ...string) QueryOption
func WithTools(tools ...string) QueryOption
func WithToolsPreset() QueryOption               // Use claude_code default tools
func WithPermissionMode(mode PermissionMode) QueryOption
func WithBypassPermissions() QueryOption          // Requires WithAllowDangerouslySkipPermissions
func WithAllowDangerouslySkipPermissions() QueryOption
func WithCanUseTool(fn CanUseToolFunc) QueryOption

// System Prompt
func WithSystemPrompt(prompt string) QueryOption
func WithSystemPromptPreset(append string) QueryOption  // claude_code preset + append
func WithSystemPromptFile(path string) QueryOption

// Conversation
func WithResume(sessionID string) QueryOption
func WithContinue() QueryOption
func WithSessionID(id string) QueryOption
func WithForkSession() QueryOption
func WithMaxTurns(n int) QueryOption
func WithMaxBudgetUSD(budget float64) QueryOption

// Environment
func WithCwd(dir string) QueryOption
func WithEnv(env map[string]string) QueryOption
func WithAdditionalDirectories(dirs ...string) QueryOption
func WithCLIPath(path string) QueryOption
func WithSettingSources(sources ...SettingSource) QueryOption

// Extensions
func WithMcpServers(servers map[string]McpServerConfig) QueryOption
func WithAgents(agents map[string]AgentDefinition) QueryOption
func WithHooks(hooks map[HookEvent][]HookMatcher) QueryOption
func WithPlugins(plugins ...PluginConfig) QueryOption

// Output
func WithOutputFormat(schema map[string]any) QueryOption  // JSON schema for structured output
func WithIncludePartialMessages() QueryOption

// Debugging
func WithStderr(fn func(string)) QueryOption
func WithDebug() QueryOption
func WithDebugFile(path string) QueryOption
func WithMaxBufferSize(size int) QueryOption
```

### Message Types

```go
// SDKMessage is a tagged union of all message types from the claude process.
type SDKMessage struct {
    Type string          // "user", "assistant", "system", "result", "stream_event", "rate_limit_event"
    Raw  json.RawMessage // Original JSON for advanced use cases
}

// Type assertion methods (following anthropic-sdk-go union pattern)
func (m SDKMessage) AsUser() (*UserMessage, bool)
func (m SDKMessage) AsAssistant() (*AssistantMessage, bool)
func (m SDKMessage) AsSystem() (*SystemMessage, bool)
func (m SDKMessage) AsResult() (*ResultMessage, bool)
func (m SDKMessage) AsStreamEvent() (*StreamEvent, bool)
func (m SDKMessage) AsRateLimitEvent() (*RateLimitEvent, bool)

// Concrete message types
type UserMessage struct {
    Content          any     `json:"content"` // string or []ContentBlock
    UUID             string  `json:"uuid"`
    SessionID        string  `json:"session_id"`
    ParentToolUseID  *string `json:"parent_tool_use_id"`
}

type AssistantMessage struct {
    Content          []ContentBlock  `json:"content"`
    Model            string          `json:"model"`
    StopReason       string          `json:"stop_reason"`
    UUID             string          `json:"uuid"`
    SessionID        string          `json:"session_id"`
    ParentToolUseID  *string         `json:"parent_tool_use_id"`
    Usage            *MessageUsage   `json:"usage"`
}

type SystemMessage struct {
    Subtype   string  `json:"subtype"`
    UUID      string  `json:"uuid"`
    SessionID string  `json:"session_id"`
    // Subtype-specific fields populated based on subtype value
}

type ResultMessage struct {
    Subtype      string   `json:"subtype"`
    SessionID    string   `json:"session_id"`
    DurationMs   int      `json:"duration_ms"`
    DurationAPIMs int     `json:"duration_api_ms"`
    IsError      bool     `json:"is_error"`
    NumTurns     int      `json:"num_turns"`
    TotalCostUSD *float64 `json:"total_cost_usd"`
    Result       string   `json:"result"`
    StopReason   string   `json:"stop_reason"`
    Usage        *ResultUsage `json:"usage"`
}

// Content blocks
type ContentBlock struct {
    Type string `json:"type"` // "text", "thinking", "tool_use", "tool_result"
}
func (b ContentBlock) AsText() (*TextBlock, bool)
func (b ContentBlock) AsThinking() (*ThinkingBlock, bool)
func (b ContentBlock) AsToolUse() (*ToolUseBlock, bool)
func (b ContentBlock) AsToolResult() (*ToolResultBlock, bool)

type TextBlock struct {
    Text string `json:"text"`
}

type ThinkingBlock struct {
    Thinking  string `json:"thinking"`
    Signature string `json:"signature"`
}

type ToolUseBlock struct {
    ID    string         `json:"id"`
    Name  string         `json:"name"`
    Input map[string]any `json:"input"`
}

type ToolResultBlock struct {
    ToolUseID string `json:"tool_use_id"`
    Content   any    `json:"content"` // string or []ContentBlock
    IsError   *bool  `json:"is_error"`
}
```

### Transport Layer

```go
// Transport abstracts the communication with the claude process.
// The default implementation spawns a subprocess, but custom implementations
// can connect to remote claude instances.
type Transport interface {
    Connect(ctx context.Context) error
    Write(data string) error
    ReadMessages() <-chan json.RawMessage
    Close() error
    EndInput() error
    IsReady() bool
}
```

**SubprocessTransport** implementation:

1. **CLI discovery**: Checks bundled path, then `$PATH`, then standard locations (`~/.claude/local/claude`, `/usr/local/bin/claude`, etc.)
2. **Process spawn**: `exec.CommandContext(ctx, cliPath, args...)` with stdin=pipe, stdout=pipe, stderr=pipe
3. **CLI arguments**: Maps `queryConfig` to `--output-format stream-json --verbose --input-format stream-json` plus all option-specific flags (see Python SDK's `_build_command()` for full mapping)
4. **stdout parsing**: Line-by-line JSON parsing with buffer for partial messages (max 1MB default)
5. **Graceful shutdown**: Close stdin → wait 5s → SIGTERM → wait 2s → SIGKILL

### Control Protocol

Bidirectional JSON messages on stdin/stdout for SDK ↔ CLI interaction:

**SDK → CLI (control requests):**
- `initialize`: Register hooks, agents, SDK MCP servers
- `interrupt`: Interrupt current execution
- `set_permission_mode`, `set_model`: Runtime config changes
- `mcp_status`, `mcp_toggle`, `mcp_reconnect`: MCP management
- `stop_task`: Stop background task

**CLI → SDK (control requests):**
- `can_use_tool`: Permission callback (if `WithCanUseTool` set)
- `hook_callback`: Hook execution callback
- `mcp_message`: SDK MCP server message routing

### Hooks

```go
type HookEvent string
const (
    HookPreToolUse          HookEvent = "PreToolUse"
    HookPostToolUse         HookEvent = "PostToolUse"
    HookPostToolUseFailure  HookEvent = "PostToolUseFailure"
    HookUserPromptSubmit    HookEvent = "UserPromptSubmit"
    HookStop                HookEvent = "Stop"
    HookSubagentStop        HookEvent = "SubagentStop"
    HookPreCompact          HookEvent = "PreCompact"
    HookNotification        HookEvent = "Notification"
    HookSubagentStart       HookEvent = "SubagentStart"
    HookPermissionRequest   HookEvent = "PermissionRequest"
)

type HookCallback func(ctx context.Context, input HookInput, toolUseID string) (HookOutput, error)

type HookMatcher struct {
    Matcher  string         // Pattern to match (e.g., tool name regex)
    Hooks    []HookCallback
    Timeout  time.Duration  // 0 = default (30s)
}
```

### MCP Server Support

```go
// External MCP server (stdio, SSE, or HTTP)
type McpStdioServerConfig struct {
    Command string            `json:"command"`
    Args    []string          `json:"args,omitempty"`
    Env     map[string]string `json:"env,omitempty"`
}

type McpSSEServerConfig struct {
    URL     string            `json:"url"`
    Headers map[string]string `json:"headers,omitempty"`
}

type McpHttpServerConfig struct {
    URL     string            `json:"url"`
    Headers map[string]string `json:"headers,omitempty"`
}

// In-process SDK MCP server
type McpSdkServerConfig struct {
    Server *McpSdkServer
}

type McpSdkServer struct {
    Name    string
    Version string
    Tools   []McpTool
}

// Tool definition helper
func Tool[T any](name, description string, handler func(context.Context, T) (McpToolResult, error)) McpTool
```

### Subagent Definition

```go
type AgentDefinition struct {
    Description     string              `json:"description"`
    Prompt          string              `json:"prompt"`
    Tools           []string            `json:"tools,omitempty"`
    DisallowedTools []string            `json:"disallowedTools,omitempty"`
    Model           string              `json:"model,omitempty"` // "sonnet", "opus", "haiku", "inherit"
    McpServers      []any               `json:"mcpServers,omitempty"`
    Skills          []string            `json:"skills,omitempty"`
    MaxTurns        int                 `json:"maxTurns,omitempty"`
}
```

### Error Types

```go
// CLINotFoundError is returned when the claude binary is not found.
type CLINotFoundError struct {
    SearchedPaths []string
}

// ProcessError is returned when the claude process exits with a non-zero code.
type ProcessError struct {
    ExitCode int
    Stderr   string
}

// JSONDecodeError is returned when stdout contains invalid JSON.
type JSONDecodeError struct {
    Line          string
    OriginalError error
}

// MessageParseError is returned when a message is missing required fields.
type MessageParseError struct {
    Data json.RawMessage
    Err  error
}
```

---

## Phase 2: Foundation (Agent Cards + Discovery)

### Scope

This phase adds the server-side infrastructure for agents to register their capabilities and discover each other. Simplified to support `claudecode` type only.

This phase is largely identical to the [existing Phase 1 plan](../plans/2026-03-27-phase1-agent-cards-discovery.md) with these changes:

1. **Migration number**: `013_agent_cards.sql` (current latest is 012)
2. **Default card**: Only `claudecode` type gets default skills/tools
3. **Default skills for claudecode**: `code-editing`, `terminal`, `code-search`, `web-search`, `web-fetch`
4. **Default MCP tools for claudecode**: `Read`, `Write`, `Edit`, `Bash`, `Glob`, `Grep`

### Database

```sql
-- Migration: 013_agent_cards.sql
CREATE TABLE agent_cards (
    sandbox_id   TEXT PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
    agent_type   TEXT NOT NULL,
    agent_status TEXT NOT NULL DEFAULT 'available',
    card_json    TEXT NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_agent_cards_type ON agent_cards(agent_type);
CREATE INDEX idx_agent_cards_status ON agent_cards(agent_status);
```

### Default Card for claudecode

```go
func DefaultCardForType(agentType, name string) AgentCardData {
    if agentType != "claudecode" {
        return AgentCardData{Name: name, SupportedModes: []string{"async"}, MaxConcurrency: 1}
    }
    return AgentCardData{
        Name:        name,
        Description: "Claude Code agent with full coding, terminal, and search capabilities",
        Skills: []Skill{
            {Name: "code-editing", Description: "Read, write, and edit source code files", Tags: []string{"code", "files"}},
            {Name: "terminal", Description: "Execute shell commands", Tags: []string{"bash", "shell"}},
            {Name: "code-search", Description: "Search and navigate codebases", Tags: []string{"grep", "find"}},
            {Name: "web-search", Description: "Search the web for information", Tags: []string{"web"}},
            {Name: "web-fetch", Description: "Fetch and parse web page content", Tags: []string{"web"}},
        },
        MCPTools: []MCPTool{
            {Name: "Read", Description: "Read file contents"},
            {Name: "Write", Description: "Create new files"},
            {Name: "Edit", Description: "Make precise edits to existing files"},
            {Name: "Bash", Description: "Execute shell commands"},
            {Name: "Glob", Description: "Find files by pattern"},
            {Name: "Grep", Description: "Search file contents with regex"},
        },
        SupportedModes: []string{"async"},
        MaxConcurrency: 1,
    }
}
```

### API Endpoints

All under `/api/agent/discovery/` with `agentAuthMiddleware` (proxy_token auth):

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/agent/discovery/cards` | Register/update agent card |
| GET | `/api/agent/discovery/agents` | List agents (filter by type/status/skill/tag) |
| GET | `/api/agent/discovery/agents/{sandbox_id}` | Get single agent card with MCP tools |

### AgentHealthMonitor

Background goroutine (30s sweep interval, 60s offline threshold). Marks agents as `offline` when heartbeat lapses. Started in `cmd/serve.go` alongside `idleWatcher`.

### Files

| Action | File | Description |
|--------|------|-------------|
| Create | `internal/db/migrations/013_agent_cards.sql` | Schema |
| Create | `internal/db/agent_cards.go` | Types + CRUD |
| Create | `internal/server/agent_auth.go` | AgentAuthMiddleware |
| Create | `internal/server/agent_discovery.go` | Handlers + matching logic |
| Create | `internal/server/agent_discovery_test.go` | Unit tests |
| Create | `internal/server/agent_health.go` | AgentHealthMonitor |
| Modify | `internal/server/server.go` | Wire routes |
| Modify | `internal/sandboxproxy/tunnel.go` | Extended heartbeat + auto-register |
| Modify | `cmd/serve.go` | Start/stop health monitor |

---

## Phase 3: Task Delegation

### Task Execution Model

When an agent receives a delegated task, the agentserver-agent process uses the **Go Claude Agent SDK** to execute it:

```go
// internal/agent/task_executor.go
type TaskExecutor struct {
    workdir string
    apiKey  string  // ANTHROPIC_API_KEY for Claude API access
}

func (e *TaskExecutor) Execute(ctx context.Context, task *TaskRequest) (*TaskResult, error) {
    stream := agentsdk.Query(ctx, task.BuildPrompt(),
        agentsdk.WithCwd(e.workdir),
        agentsdk.WithPermissionMode("bypassPermissions"),
        agentsdk.WithAllowDangerouslySkipPermissions(),
        agentsdk.WithAllowedTools("Read", "Write", "Edit", "Bash", "Glob", "Grep"),
        agentsdk.WithMaxTurns(50),
        agentsdk.WithMaxBudgetUSD(5.0),
        agentsdk.WithSystemPrompt(task.SystemContext()),
    )

    var lastProgress time.Time
    for stream.Next() {
        msg := stream.Current()
        // Report progress every 10 seconds
        if time.Since(lastProgress) > 10*time.Second {
            e.reportProgress(ctx, task.ID, msg)
            lastProgress = time.Now()
        }
    }
    if err := stream.Err(); err != nil {
        return nil, fmt.Errorf("task execution failed: %w", err)
    }

    result, err := stream.Result()
    if err != nil {
        return nil, err
    }
    return &TaskResult{
        Output:  result.Result,
        CostUSD: result.TotalCostUSD,
    }, nil
}
```

### Task Prompt Construction

```go
func (t *TaskRequest) BuildPrompt() string {
    var sb strings.Builder
    sb.WriteString(fmt.Sprintf("You are executing a delegated task from agent %q.\n\n", t.RequesterName))
    if t.Skill != "" {
        sb.WriteString(fmt.Sprintf("Skill requested: %s\n\n", t.Skill))
    }
    sb.WriteString("Task input:\n")
    // Marshal t.Input as formatted JSON
    inputJSON, _ := json.MarshalIndent(t.Input, "", "  ")
    sb.Write(inputJSON)
    sb.WriteString("\n\nExecute this task thoroughly and report the result.")
    return sb.String()
}
```

### Database

```sql
-- Migration: 014_agent_tasks.sql
CREATE TABLE agent_tasks (
    id                TEXT PRIMARY KEY,
    workspace_id      TEXT NOT NULL REFERENCES workspaces(id),
    requester_id      TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    target_id         TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    skill             TEXT,
    input_json        TEXT NOT NULL,
    output_json       TEXT,
    status            TEXT NOT NULL DEFAULT 'pending',
    mode              TEXT NOT NULL DEFAULT 'async',
    failure_reason    TEXT,
    timeout_seconds   INTEGER DEFAULT 300,
    delegation_chain  TEXT NOT NULL DEFAULT '[]',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    accepted_at       TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ
);

CREATE INDEX idx_agent_tasks_workspace ON agent_tasks(workspace_id);
CREATE INDEX idx_agent_tasks_requester ON agent_tasks(requester_id);
CREATE INDEX idx_agent_tasks_target_status ON agent_tasks(target_id, status);
CREATE INDEX idx_agent_tasks_cleanup ON agent_tasks(status, completed_at);
```

### API Endpoints

All under `/api/agent/` with `agentAuthMiddleware`:

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/agent/tasks` | Create delegation (sync/async, by ID or skill) |
| GET | `/api/agent/tasks/{task_id}` | Get task status and result |
| GET | `/api/agent/tasks` | List requester's tasks (filter by status) |

### Task Delivery

**Cloud agents** — HTTP POST to `http://{pod_ip}:{task_port}/agent/tasks`:

```json
{
  "task_id": "task-123",
  "requester_id": "sandbox-a",
  "requester_name": "My Claude Code",
  "skill": "code-review",
  "input": { "path": "/src/server", "focus": "bugs" }
}
```

**Local agents** — New tunnel frame types:

```go
// internal/tunnel/stream.go
const (
    StreamTypeHTTP          byte = 0x01
    StreamTypeTerminal      byte = 0x02
    StreamTypeControl       byte = 0x03
    StreamTypeTaskRequest   byte = 0x04  // server → agent
    StreamTypeTaskResponse  byte = 0x05  // agent → server
)
```

Task request frame payload:
```json
{
  "task_id": "task-123",
  "requester_id": "sandbox-a",
  "requester_name": "My Claude Code",
  "skill": "code-review",
  "input": { ... }
}
```

Task response frame payload:
```json
{
  "task_id": "task-123",
  "status": "accepted|running|completed|failed",
  "output": { ... },
  "failure_reason": "..."
}
```

Agent client handling — extend `handleServerStream()` in `internal/agent/client.go`:

```go
case tunnel.StreamTypeTaskRequest:
    go c.handleTaskRequest(ctx, stream, metadata)
```

### Sync Mode

The server holds the HTTP connection open using a blocking channel. A background goroutine monitors task status changes. Hard timeout: `min(request.timeout, 600)` seconds. If the connection drops, the task continues — requester polls via GET.

### Loop Detection

Each task carries `delegation_chain` (JSON array of sandbox IDs). Before creating a task, the server checks if `target_id` appears in the chain. Max depth: 5.

### Task Overflow

When an agent is at `max_concurrency`, tasks are rejected with `at_capacity`. No server-side queuing.

### Task Cleanup

Background `TaskCleanupWorker` runs daily. Deletes completed/failed tasks older than 7 days. The `agent_interactions` audit table (Phase 5) is not cleaned.

### Files

| Action | File | Description |
|--------|------|-------------|
| Create | `internal/db/migrations/014_agent_tasks.sql` | Schema |
| Create | `internal/db/agent_tasks.go` | Types + CRUD |
| Create | `internal/server/agent_tasks.go` | Handlers (create, get, list) |
| Create | `internal/server/agent_tasks_test.go` | Unit tests |
| Create | `internal/server/task_cleanup.go` | TaskCleanupWorker goroutine |
| Create | `internal/agent/task_executor.go` | TaskExecutor using Go Agent SDK |
| Modify | `internal/tunnel/stream.go` | Add StreamTypeTaskRequest/Response |
| Modify | `internal/agent/client.go` | Handle task_request frames |
| Modify | `internal/sandboxproxy/tunnel.go` | Handle task_response frames |
| Modify | `internal/server/server.go` | Wire task routes |
| Modify | `cmd/serve.go` | Start/stop TaskCleanupWorker |

---

## Phase 4: MCP Integration + Tool Injection

### agentserver-mcp-bridge

A standalone Go binary that acts as a stdio MCP server. Claude Code connects to it via MCP configuration, and it proxies `discover_agents`, `delegate_task`, and `check_task` tools to the agentserver HTTP API.

```
cmd/agentserver-mcp-bridge/
├── main.go                 // stdio server, JSON-RPC 2.0
internal/mcpbridge/
├── bridge.go               // MCP server implementation
└── tools.go                // discover_agents, delegate_task, check_task handlers
```

**How it's configured in Claude Code** (via MCP server config):

```json
{
  "mcpServers": {
    "agentserver": {
      "command": "/usr/local/bin/agentserver-mcp-bridge",
      "env": {
        "AGENTSERVER_URL": "https://agentserver.example.com",
        "AGENTSERVER_TOKEN": "<proxy_token>"
      }
    }
  }
}
```

**For cloud agents**: The bridge binary is included in the container image. The MCP config is injected via `OPENCODE_CONFIG_CONTENT` (or equivalent Claude Code config mechanism) during container creation.

**For local agents**: The agentserver-agent process starts a local bridge instance when it receives the `inject_tools` tunnel frame.

### inject_tools Tunnel Frame

Server sends `inject_tools` text frame to local agents:
1. On tunnel connection establishment
2. When workspace agent topology changes

```json
{
  "type": "inject_tools",
  "tools": [
    {
      "name": "discover_agents",
      "description": "Discover other agents in this workspace...",
      "input_schema": { ... }
    },
    {
      "name": "delegate_task",
      "description": "Delegate a task to another agent...",
      "input_schema": { ... }
    },
    {
      "name": "check_task",
      "description": "Check the status of a delegated task...",
      "input_schema": { ... }
    }
  ],
  "api_base_url": "https://agentserver.example.com/api/agent",
  "auth_token": "<proxy_token>"
}
```

**Agent-side handling** (in `internal/agent/client.go`):
1. Parse `inject_tools` control frame
2. Start local `agentserver-mcp-bridge` process with the provided URL and token
3. Configure Claude Code to connect to this MCP server

### Injected MCP Tools

```json
{
  "name": "discover_agents",
  "description": "Discover other agents in this workspace by skill, tags, or type.",
  "input_schema": {
    "type": "object",
    "properties": {
      "skill": { "type": "string" },
      "tags": { "type": "array", "items": { "type": "string" } },
      "type": { "type": "string", "enum": ["claudecode"] },
      "status": { "type": "string", "enum": ["available", "busy"] },
      "limit": { "type": "integer", "default": 10 }
    }
  }
}
```

```json
{
  "name": "delegate_task",
  "description": "Delegate a task to another agent. Use discover_agents first to find a suitable agent.",
  "input_schema": {
    "type": "object",
    "properties": {
      "target_id": { "type": "string", "description": "Target agent's sandbox ID" },
      "skill": { "type": "string", "description": "Skill to invoke" },
      "input": { "type": "object", "description": "Task input data" },
      "mode": { "type": "string", "enum": ["sync", "async"], "default": "async" }
    },
    "required": ["target_id", "input"]
  }
}
```

```json
{
  "name": "check_task",
  "description": "Check the status and result of a previously delegated task.",
  "input_schema": {
    "type": "object",
    "properties": {
      "task_id": { "type": "string" }
    },
    "required": ["task_id"]
  }
}
```

### Server-side MCP Proxy

For future use when task executors need to call tools on peer agents:

```
Agent A calls tool "agent/{sandbox-b-id}/read_file"
  → Server parses "agent/{id}/{tool}" prefix
  → Server validates: both agents in same workspace
  → Local agents: forwards via WebSocket tunnel (new frame type)
  → Cloud agents: forwards via HTTP POST to pod-ip:port/mcp/tools/call
  → Server relays result back to Agent A
```

This is Phase 4+ infrastructure and can be deferred if task delegation (Phase 3) is sufficient for initial use cases.

### Files

| Action | File | Description |
|--------|------|-------------|
| Create | `cmd/agentserver-mcp-bridge/main.go` | MCP bridge binary entry point |
| Create | `internal/mcpbridge/bridge.go` | MCP server implementation |
| Create | `internal/mcpbridge/tools.go` | Tool handlers |
| Modify | `internal/agent/client.go` | Handle inject_tools frame, start local bridge |
| Modify | `internal/sandboxproxy/tunnel.go` | Send inject_tools frame on connect |
| Modify | `Dockerfile` | Include agentserver-mcp-bridge binary |

---

## Phase 5: Security + Observability

### JWT Fast-Path (Cloud-to-Cloud Direct Calls)

For MCP tool calls between cloud agents in the same K8s namespace:

1. Agent A requests direct access to Agent B
2. Server issues short-lived JWT (5min TTL, HMAC-SHA256):
   ```json
   {
     "requester_id": "sandbox-a",
     "target_id": "sandbox-b",
     "workspace_id": "ws-123",
     "allowed_tools": ["Read", "Grep"],
     "jti": "unique-jwt-id",
     "exp": 1711382400
   }
   ```
3. Server returns target pod IP + port + JWT
4. Agent A calls target directly
5. Target validates JWT signature, checks fields

**Signing key**: `AGENTSERVER_JWT_SECRET` env var, distributed to cloud agents during container creation.

### Audit Logging

```sql
-- Migration: 015_agent_interactions.sql
-- No FK constraints: audit records must survive entity deletion
CREATE TABLE agent_interactions (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL,
    requester_id     TEXT NOT NULL,
    target_id        TEXT NOT NULL,
    interaction_type TEXT NOT NULL,  -- "discovery" | "task_create" | "task_complete" | "mcp_tool_call"
    detail_json      TEXT,
    status           TEXT NOT NULL,  -- "success" | "failed" | "rejected"
    duration_ms      INTEGER,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_agent_interactions_workspace_time ON agent_interactions(workspace_id, created_at);

-- Workspace delegation mode
ALTER TABLE workspaces ADD COLUMN delegation_mode TEXT NOT NULL DEFAULT 'auto';
```

### Audit Middleware

Wraps all `/api/agent/` handlers. Logs interaction type, requester, target, status, and duration. Does not log full payloads.

### Workspace Delegation Mode

| Mode | Behavior |
|------|----------|
| `auto` | Tasks delivered to targets immediately |
| `approval` | Tasks held in `pending` until workspace member approves via UI |

### Rate Limiting

- Per-agent: 10 tasks/min default
- Per-workspace: 50 tasks/min aggregate
- Prevents delegation loops from consuming resources

### Dashboard API

New endpoints under the existing cookie-auth `/api/` group:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/workspaces/{wid}/agents` | List all agents with cards |
| GET | `/api/workspaces/{wid}/agent-tasks` | Task history with pagination |
| GET | `/api/workspaces/{wid}/agent-interactions` | Audit log with pagination |

### Files

| Action | File | Description |
|--------|------|-------------|
| Create | `internal/db/migrations/015_agent_interactions.sql` | Audit table + delegation mode |
| Create | `internal/server/agent_jwt.go` | JWT issuance + validation |
| Create | `internal/server/audit.go` | Audit logging middleware |
| Create | `internal/db/agent_interactions.go` | Audit table CRUD |
| Modify | `internal/db/workspaces.go` | Add delegation_mode field |
| Modify | `internal/server/server.go` | Wire dashboard routes, audit middleware |
| Modify | `internal/server/agent_tasks.go` | Add rate limiting, approval mode |

---

## End-to-End Example

**Scenario**: User tells local Claude Code to review Go code, and another Claude Code agent specializing in Go is available.

```
1. User → Local Claude Code: "Review the Go code in /src/server"

2. Local Claude Code AI model calls discover_agents({ skill: "code-review", tags: ["go"] })
   → agentserver-mcp-bridge proxies to GET /api/agent/discovery/agents?skill=code-review&tag=go

3. Server returns:
   [{ agent_id: "cloud-reviewer", name: "Go Expert", status: "available",
      skills: [{ name: "code-review", tags: ["go", "security"] }] }]

4. AI model calls delegate_task({
     target_id: "cloud-reviewer",
     skill: "code-review",
     input: { path: "/src/server", focus: "bugs and security" },
     mode: "async"
   })
   → Bridge proxies to POST /api/agent/tasks

5. Server creates task, delivers to cloud-reviewer via HTTP
   → Cloud-reviewer's agentserver-agent receives task
   → TaskExecutor uses Go Agent SDK:
     agentsdk.Query(ctx, buildPrompt(task), WithCwd(...), ...)
   → Claude autonomously reviews the code
   → Task completes with findings

6. Local AI model calls check_task({ task_id: "task-123" })
   → Gets review results

7. AI presents findings to user:
   "The Go Expert found 3 issues: ..."
```

---

## Cross-Cutting Concerns

### Authentication Summary

| Operation | Auth Method |
|-----------|-------------|
| Discovery (list agents) | proxy_token (AgentAuthMiddleware) |
| Card registration | proxy_token |
| Task creation | proxy_token |
| MCP proxy call | proxy_token |
| Direct fast-path (K8s) | Short-lived JWT (5min) |
| Dashboard API | Cookie (existing auth) |

### Error Handling

- Agent goes offline during task → task marked `failed` with reason `agent_offline`
- Task timeout → task marked `failed` with reason `timeout`
- Delegation loop detected → task rejected with reason `delegation_loop_detected`
- Agent at capacity → task rejected with reason `at_capacity`
- Claude API error during execution → task marked `failed` with SDK error details

### Data Retention

- Agent cards: Retained while sandbox exists (CASCADE DELETE)
- Agent tasks: 7-day retention after completion
- Agent interactions: Permanent audit trail (no FK, no cleanup)
