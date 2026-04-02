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
| SDK scope | Full feature parity with TS/Python SDKs | Foundation for all subsequent phases; worth investing time upfront |
| Cloud task receiver | Go process built with Agent SDK | Same SDK used for both task reception and execution in cloud containers |
| Cloud LLM auth | Via existing llmproxy | Cloud agents route Claude API calls through agentserver's llmproxy |
| Local LLM auth | User-provided (for now) | User's own claude CLI auth; agentserver may provide this in future |
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
- Phase 1 complete: Go Agent SDK usable independently, feature parity with TS/Python SDKs
- Phase 2 complete: agents can register capabilities and discover each other
- Phase 3 complete: task delegation infrastructure works (API + delivery + execution); external programs can create tasks via HTTP API, but the AI model cannot yet autonomously delegate (no MCP tools injected)
- Phase 4 complete: AI model autonomously discovers and delegates via injected MCP tools; this is the phase that makes Phase 2+3 usable end-to-end
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

### Design Inspiration: Claude Code's Agent Discovery

Claude Code 的 Agent 发现策略有以下值得借鉴的设计：

1. **能力声明即发现依据**：CC 的 Agent 通过 frontmatter 声明 description（给 AI 看的选择指南）、tools、skills、model、permissionMode 等。发现不是简单的 name 匹配，而是多维能力匹配。
2. **依赖就绪检查**：CC 的 `filterAgentsByMcpRequirements()` 在返回可用 Agent 前检查其声明的 MCP 依赖是否就绪。Agent 不仅要"在线"，还要"能力完备"。
3. **自动注册**：CC 的 Agent 定义从文件系统自动加载，不需要显式 API 调用注册。

以下 CC 设计不适用于 agentserver（以及原因）：

| CC 设计 | 不适用原因 |
|---------|-----------|
| 四层优先级覆盖链 | agentserver 每个 sandbox 只有一张 card，不存在多源冲突 |
| Built-in Agent 类型 | agentserver 不内置 Agent，所有 Agent 均用户注册 |
| agent_listing_delta 注入 | agentserver 通过 API 发现，不需要注入系统提示词 |
| Fork path | agentserver Agent 间无共享上下文 |
| Skill 加载系统 | agentserver Agent 能力通过 card 声明，无需独立 skill 机制 |

### Scope

This phase adds the server-side infrastructure for agents to register their capabilities and discover each other. First iteration targets `claudecode` type only; `agent_type` field retained for future extensibility.

### Database

```sql
-- Migration: 013_agent_cards.sql
CREATE TABLE agent_cards (
    sandbox_id   TEXT PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
    agent_type   TEXT NOT NULL,
    agent_status TEXT NOT NULL DEFAULT 'available',
    -- agent_status: 'available' | 'busy' | 'offline'
    card_json    TEXT NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_agent_cards_type ON agent_cards(agent_type);
CREATE INDEX idx_agent_cards_status ON agent_cards(agent_status);
```

### Agent Card Data Model

借鉴 CC 的 Agent frontmatter 结构，Card 包含能力边界和运行约束：

```go
// internal/db/agent_cards.go

type AgentCardData struct {
    // --- 身份 ---
    Name        string `json:"name"`
    Description string `json:"description"` // AI 模型选择时看到的简短描述（类比 CC 的 whenToUse）

    // --- 能力声明 ---
    Skills   []Skill   `json:"skills,omitempty"`
    MCPTools []MCPTool `json:"mcp_tools,omitempty"`
    Tags     []string  `json:"tags,omitempty"` // 自由标签（"go", "security", "frontend"）

    // --- 运行约束 ---
    SupportedModes []string `json:"supported_modes"` // ["async"], ["async", "sync"]
    MaxConcurrency int      `json:"max_concurrency"` // 并发任务上限
    MaxTurns       int      `json:"max_turns,omitempty"`       // 单任务最大轮次
    MaxBudgetUSD   float64  `json:"max_budget_usd,omitempty"`  // 单任务预算上限
    Model          string   `json:"model,omitempty"`           // 使用的模型（信息性字段）
    Isolation      string   `json:"isolation,omitempty"`       // "none", "worktree"

    // --- 依赖声明（借鉴 CC 的 requiredMcpServers）---
    // 发现 API 只返回所有依赖就绪的 Agent。
    // 支持通配符匹配：["database-*", "auth-server"]
    RequiredServices []string `json:"required_services,omitempty"`
}

type Skill struct {
    Name        string   `json:"name"`
    Description string   `json:"description"`
    Tags        []string `json:"tags,omitempty"`
}

type MCPTool struct {
    Name        string `json:"name"`
    Description string `json:"description"`
}
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
        Tags:           []string{"code", "terminal", "search"},
        SupportedModes: []string{"async"},
        MaxConcurrency: 1,
        MaxTurns:       50,
        MaxBudgetUSD:   5.0,
    }
}
```

### Auto-Registration via Tunnel Heartbeat

借鉴 CC 的自动加载设计：本地 Agent 连接 tunnel 时自动注册/更新 Card，无需显式 API 调用。

```go
// internal/sandboxproxy/tunnel.go — 扩展心跳处理

func (s *Server) handleTunnelHeartbeat(sandboxID string, data json.RawMessage) {
    // 1. 更新 sandbox 心跳（已有逻辑）
    s.DB.UpdateSandboxHeartbeat(sandboxID)

    // 2. 检查是否携带 card 数据（新增）
    var heartbeat struct {
        AgentInfo json.RawMessage `json:"agent_info,omitempty"`
        Card      *AgentCardData  `json:"card,omitempty"`      // Agent 自带 card
        CardHash  string          `json:"card_hash,omitempty"` // card 内容哈希，变更时才更新
    }
    json.Unmarshal(data, &heartbeat)

    if heartbeat.Card != nil {
        // 仅在 card 内容变更时更新 DB（通过 hash 判断）
        existing, _ := s.DB.GetAgentCard(sandboxID)
        if existing == nil || existing.CardHash != heartbeat.CardHash {
            s.DB.UpsertAgentCard(sandboxID, heartbeat.Card)
        }
    }
}
```

Agent 侧：从本地配置文件 `~/.agent/card.yaml` 加载 card 数据，每次心跳携带：

```yaml
# ~/.agent/card.yaml — 本地 Agent 能力声明
name: "Go Expert"
description: "Specialized in Go code review, testing, and performance optimization"
skills:
  - name: code-review
    description: "Review Go code for bugs, security issues, and best practices"
    tags: [go, security, review]
  - name: testing
    description: "Write and run Go tests"
    tags: [go, test]
tags: [go, backend, senior]
max_concurrency: 2
max_turns: 100
max_budget_usd: 10.0
```

### Discovery API

All under `/api/agent/discovery/` with `agentAuthMiddleware` (proxy_token auth):

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/agent/discovery/cards` | Register/update agent card (显式 API，云端 Agent 使用) |
| GET | `/api/agent/discovery/agents` | 多维发现（见下方） |
| GET | `/api/agent/discovery/agents/{sandbox_id}` | 获取单个 Agent card |

#### 多维发现查询

借鉴 CC 的能力匹配设计，发现 API 支持服务端多维过滤（不是客户端遍历）：

```
GET /api/agent/discovery/agents?skill=code-review&tag=go&status=available&mode=async&limit=10
```

| 参数 | 类型 | 说明 |
|------|------|------|
| `skill` | string | 按 skill name 匹配（模糊匹配） |
| `tag` | string | 按 tag 匹配（精确，可多个：`tag=go&tag=security`） |
| `type` | string | 按 agent_type 过滤（默认 `claudecode`） |
| `status` | string | `available`, `busy`, `offline`（默认 `available`） |
| `mode` | string | `async`, `sync` |
| `limit` | int | 最大返回数（默认 10） |
| `exclude` | string | 排除的 sandbox_id（避免自己发现自己） |

```go
// internal/server/agent_discovery.go

func (s *Server) handleDiscoverAgents(w http.ResponseWriter, r *http.Request) {
    q := r.URL.Query()
    filters := DiscoveryFilters{
        Skill:   q.Get("skill"),
        Tags:    q["tag"],
        Type:    q.Get("type"),
        Status:  q.Get("status"),
        Mode:    q.Get("mode"),
        Limit:   parseIntOr(q.Get("limit"), 10),
        Exclude: q.Get("exclude"),
    }

    agents, err := s.DB.DiscoverAgents(r.Context(), filters)
    // ...

    // 借鉴 CC 的 filterAgentsByMcpRequirements：
    // 过滤掉 required_services 未满足的 Agent
    agents = filterByServiceReadiness(agents, s.getAvailableServices())

    json.NewEncoder(w).Encode(agents)
}
```

#### 发现结果格式

返回值格式参考 CC 给 AI 模型的 Agent 列表，简洁且信息充分：

```json
[
  {
    "agent_id": "sandbox-abc123",
    "name": "Go Expert",
    "description": "Specialized in Go code review, testing, and performance optimization",
    "skills": [
      {"name": "code-review", "description": "Review Go code for bugs...", "tags": ["go", "security"]}
    ],
    "tags": ["go", "backend", "senior"],
    "status": "available",
    "model": "opus",
    "supported_modes": ["async"],
    "max_concurrency": 2,
    "current_tasks": 0
  }
]
```

注意 `current_tasks` 字段：让调用方知道 Agent 当前的负载，以便做出更好的选择。

### AgentHealthMonitor

Background goroutine (30s sweep interval, 60s offline threshold).

```go
// internal/server/agent_health.go

type AgentHealthMonitor struct {
    db       *db.DB
    interval time.Duration // 30s
    offline  time.Duration // 60s
}

func (m *AgentHealthMonitor) Run(ctx context.Context) {
    ticker := time.NewTicker(m.interval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // 1. 标记心跳过期的 Agent 为 offline
            m.db.MarkOfflineAgents(m.offline)
            // 2. 清除已删除 sandbox 的残留 card（CASCADE 已处理，这是兜底）
        }
    }
}
```

### Card 文件格式（本地 Agent）

本地 Agent 使用 YAML 文件声明能力（借鉴 CC 的 `.claude/agents/*.md` frontmatter 模式）：

```
~/.agent/
├── registry.json    # 已有：连接信息
└── card.yaml        # 新增：能力声明
```

`card.yaml` 在 Agent 启动时加载，通过 tunnel 心跳同步到服务端。用户修改 `card.yaml` 后，下次心跳（≤20s）自动生效。

### Cloud Agent Card 注册

云端 Agent 在容器启动时通过 HTTP API 显式注册：

```go
// cmd/agentserver-task-runner/main.go — 容器启动时

func registerCard(serverURL, proxyToken string) error {
    card := DefaultCardForType("claudecode", hostname)
    // 容器可通过环境变量覆盖默认 card：
    if custom := os.Getenv("AGENT_CARD_JSON"); custom != "" {
        json.Unmarshal([]byte(custom), &card)
    }
    // POST /api/agent/discovery/cards
    return postJSON(serverURL+"/api/agent/discovery/cards", proxyToken, card)
}
```

### Files

| Action | File | Description |
|--------|------|-------------|
| Create | `internal/db/migrations/013_agent_cards.sql` | Schema |
| Create | `internal/db/agent_cards.go` | Types + CRUD + DiscoverAgents 多维查询 |
| Create | `internal/server/agent_auth.go` | AgentAuthMiddleware (proxy_token) |
| Create | `internal/server/agent_discovery.go` | 发现 API handlers + 能力匹配 + service readiness filter |
| Create | `internal/server/agent_discovery_test.go` | Unit tests |
| Create | `internal/server/agent_health.go` | AgentHealthMonitor |
| Modify | `internal/server/server.go` | Wire routes |
| Modify | `internal/sandboxproxy/tunnel.go` | 心跳扩展：解析 card 数据，自动 upsert |
| Modify | `internal/agent/client.go` | 加载 ~/.agent/card.yaml，心跳时携带 card |
| Modify | `cmd/serve.go` | Start/stop health monitor |

---

## Phase 3: Task Delegation with Bridge Transport

### Design Inspiration: Claude Code's Remote Agent Architecture

Claude Code uses a **session-based bridge transport** for remote agent communication. The key insight is that task execution is not a one-shot fire-and-forget operation — it's a **persistent session** with real-time bidirectional communication. This allows:

- Real-time streaming of task output (not polling)
- Runtime control (interrupt, model change, permission forwarding)
- Graceful reconnection after network failures
- Epoch-based worker registration to prevent stale connections
- UUID-based message deduplication to prevent echo/replay

We adopt this architecture for agentserver, with a **unified transport abstraction** that works over both yamux streams (local agents) and SSE+HTTP (cloud agents).

### Bridge Transport Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                       agentserver                            │
│                                                              │
│  ┌──────────────┐   ┌───────────────────────────────────┐   │
│  │ TaskSession   │   │ BridgeTransport (interface)       │   │
│  │ Manager       │──→│                                   │   │
│  │              │   │  ┌─────────────┐ ┌─────────────┐  │   │
│  │ • create     │   │  │ YamuxBridge │ │  SSEBridge   │  │   │
│  │ • route      │   │  │ (local)     │ │  (cloud)     │  │   │
│  │ • gc         │   │  └──────┬──────┘ └──────┬───────┘  │   │
│  └──────────────┘   └─────────│───────────────│──────────┘   │
│                               │               │              │
└───────────────────────────────│───────────────│──────────────┘
                                │               │
              yamux stream      │               │  SSE (read) +
              (bidirectional)   │               │  HTTP POST (write)
                                │               │
┌───────────────────────────────▼───┐ ┌─────────▼──────────────┐
│  Local Agent (agentserver-agent)  │ │  Cloud Agent (K8s pod)  │
│                                   │ │                         │
│  ┌─────────────────────────────┐  │ │  ┌───────────────────┐  │
│  │ TaskWorker                  │  │ │  │ TaskWorker         │  │
│  │                             │  │ │  │                    │  │
│  │  Go Agent SDK → claude CLI  │  │ │  │ Go Agent SDK       │  │
│  │  stream messages back       │  │ │  │ → claude CLI       │  │
│  │  handle control requests    │  │ │  │ via llmproxy       │  │
│  └─────────────────────────────┘  │ │  └────────────────────┘  │
└───────────────────────────────────┘ └──────────────────────────┘
```

### Transport Interface

```go
// internal/bridge/transport.go

// BridgeTransport abstracts bidirectional communication with a remote agent.
// Two implementations: YamuxBridge (local agents via existing tunnel) and
// SSEBridge (cloud agents via SSE read + HTTP POST write).
type BridgeTransport interface {
    // Write sends a message to the worker agent.
    Write(msg *TaskMessage) error
    // WriteBatch sends multiple messages atomically.
    WriteBatch(msgs []*TaskMessage) error
    // Messages returns a channel that yields messages from the worker.
    Messages() <-chan *TaskMessage
    // SendControlRequest sends a control request and waits for response.
    SendControlRequest(ctx context.Context, req *ControlRequest) (*ControlResponse, error)
    // Close terminates the transport.
    Close() error
    // Epoch returns the current worker epoch (for stale detection).
    Epoch() int
}

// TaskMessage is the standard message envelope for bridge communication.
// Modeled after Claude Code's SDKMessage format.
type TaskMessage struct {
    Type      string          `json:"type"`       // "user", "assistant", "system", "result", "control_request", "control_response"
    Subtype   string          `json:"subtype,omitempty"`
    TaskID    string          `json:"task_id"`
    UUID      string          `json:"uuid"`       // For deduplication
    SessionID string          `json:"session_id"`
    Payload   json.RawMessage `json:"payload"`
    Timestamp time.Time       `json:"timestamp"`
}
```

### YamuxBridge (Local Agents)

Reuses the existing yamux tunnel with a new stream type:

```go
// internal/tunnel/stream.go — extend existing stream types
const (
    StreamTypeHTTP          byte = 0x01
    StreamTypeTerminal      byte = 0x02
    StreamTypeControl       byte = 0x03
    StreamTypeBridge        byte = 0x04  // NEW: bidirectional task bridge
)
```

The `StreamTypeBridge` stream is long-lived — one per active task session. Both sides can write NDJSON messages at any time. This maps directly to the yamux bidirectional stream model already used for terminals.

```go
// internal/bridge/yamux_bridge.go

type YamuxBridge struct {
    stream    net.Conn       // yamux stream (StreamTypeBridge)
    epoch     int
    msgCh     chan *TaskMessage
    writeMu   sync.Mutex
    dedup     *BoundedUUIDSet // echo/replay prevention
}

func NewYamuxBridge(tunnel *tunnel.Tunnel, taskID string) (*YamuxBridge, error) {
    // Open a new yamux stream with StreamTypeBridge type
    meta := BridgeStreamMeta{TaskID: taskID, Epoch: nextEpoch()}
    stream, err := tunnel.OpenStream(StreamTypeBridge, meta)
    // ... start read loop, return bridge
}
```

### SSEBridge (Cloud Agents)

For cloud agents without a yamux tunnel. Follows Claude Code's CCR v2 design:

```go
// internal/bridge/sse_bridge.go

type SSEBridge struct {
    taskID       string
    podIP        string
    workerToken  string
    epoch        int
    sequenceNum  int64         // SSE high-water mark for resume
    msgCh        chan *TaskMessage
    httpClient   *http.Client
    dedup        *BoundedUUIDSet
    heartbeatTk  *time.Ticker  // 20s heartbeat to maintain lease
}
```

**Read channel**: SSE stream at `http://{pod_ip}:{port}/bridge/events/stream?from_seq={n}`
- Worker pushes events as SSE frames with sequence numbers
- `Last-Event-ID` header enables resume after reconnect
- Reconnect with exponential backoff (1s → 30s, max 10 minutes)
- 45s liveness timeout

**Write channel**: HTTP POST to `http://{pod_ip}:{port}/bridge/events`
- Batched uploads (max 100 messages per batch)
- 10 retries with backoff (500ms → 8s)

**Heartbeat**: PUT `http://{pod_ip}:{port}/bridge/worker` every 20s
- Worker TTL: 60s — missing 3 heartbeats kills the session
- Reports worker state: `idle` | `running` | `requires_action`

### Message Deduplication

Adopted from Claude Code's `BoundedUUIDSet`:

```go
// internal/bridge/dedup.go

// BoundedUUIDSet is a circular buffer for UUID deduplication.
// Prevents echo (receiving our own sent messages back) and
// replay (re-processing messages after reconnect).
type BoundedUUIDSet struct {
    posted   map[string]struct{} // UUIDs we sent (echo detection)
    inbound  map[string]struct{} // UUIDs we received (replay detection)
    order    []string            // FIFO eviction order
    capacity int                 // typically 100
}

func (s *BoundedUUIDSet) IsEcho(uuid string) bool    // check posted set
func (s *BoundedUUIDSet) IsReplay(uuid string) bool   // check inbound set
func (s *BoundedUUIDSet) MarkPosted(uuid string)      // add to posted set
func (s *BoundedUUIDSet) MarkInbound(uuid string)     // add to inbound set
```

### Epoch Management

Prevents stale workers (adopted from Claude Code's epoch mechanism):

```go
// internal/bridge/epoch.go

// Worker registration bumps epoch atomically.
// Old workers with stale epochs are rejected (409 Conflict).
//
// Flow:
//   1. Worker registers → gets epoch=N
//   2. Every heartbeat includes epoch=N
//   3. New worker registers → epoch bumps to N+1
//   4. Old worker's next heartbeat → 409 → must re-register or exit
//   5. Guarantees: exactly one active worker per task session

type EpochManager struct {
    mu     sync.Mutex
    epochs map[string]int // taskID → current epoch
}

func (m *EpochManager) Register(taskID string) int           // bump & return new epoch
func (m *EpochManager) Validate(taskID string, epoch int) bool // check if epoch is current
```

### Control Protocol

Runtime control during task execution (adopted from Claude Code):

```go
// internal/bridge/control.go

type ControlRequest struct {
    RequestID string `json:"request_id"`
    Subtype   string `json:"subtype"`    // see subtypes below
    Params    any    `json:"params,omitempty"`
}

type ControlResponse struct {
    RequestID string `json:"request_id"`
    Subtype   string `json:"subtype"`    // "success" | "error"
    Result    any    `json:"result,omitempty"`
    Error     string `json:"error,omitempty"`
}
```

**Server → Worker control requests:**

| Subtype | Purpose | When |
|---------|---------|------|
| `interrupt` | Cancel the current task turn | Requester calls cancel |
| `set_model` | Change LLM model mid-task | Requester requests model change |
| `can_use_tool` | Permission check forwarding | Task needs tool approval |

**Worker → Server control requests:**

| Subtype | Purpose | When |
|---------|---------|------|
| `progress` | Real-time progress update | Tool use count, token usage changes |
| `permission_request` | Forward permission prompt to requester | Worker needs approval |

**Timeout**: Server waits 15s for control responses. No response → error.

### Task Session Lifecycle

Each delegated task creates a **TaskSession** — a persistent, resumable session:

```go
// internal/bridge/session.go

type TaskSession struct {
    ID            string          // unique session ID (ts_xxx)
    TaskID        string          // linked agent_task ID
    WorkerSandbox string          // target agent's sandbox ID
    RequesterID   string          // requesting agent's sandbox ID
    Transport     BridgeTransport // YamuxBridge or SSEBridge
    Epoch         int             // current worker epoch
    State         SessionState    // idle | running | requires_action
    CreatedAt     time.Time
    LastActivity  time.Time

    // Message history (for resume)
    transcript    []TaskMessage
    transcriptMu  sync.Mutex
}

type SessionState string
const (
    SessionIdle           SessionState = "idle"
    SessionRunning        SessionState = "running"
    SessionRequiresAction SessionState = "requires_action"
)
```

**Lifecycle:**

```
1. POST /api/agent/tasks (requester creates task)
   ↓
2. Server creates TaskSession
   ↓
3. Server establishes BridgeTransport:
   - Local agent: open StreamTypeBridge on existing yamux tunnel
   - Cloud agent: POST /bridge/sessions on pod → SSEBridge
   ↓
4. Server sends task prompt via bridge:
   { type: "user", task_id: "...", payload: { prompt, system_context, ... } }
   ↓
5. Worker receives, starts Go Agent SDK execution:
   agentsdk.Query(ctx, prompt, opts...)
   ↓
6. Worker streams messages back via bridge:
   - assistant messages (real-time)
   - system messages (tool use, progress)
   - control requests (permission checks)
   ↓
7. Worker sends result:
   { type: "result", subtype: "success", payload: { result, usage, cost } }
   ↓
8. Server updates agent_tasks status, notifies requester
   ↓
9. Session kept alive for potential follow-up turns (TTL: 5 minutes idle)
   ↓
10. Session closed, transcript archived
```

### Database

```sql
-- Migration: 014_agent_tasks.sql
CREATE TABLE agent_tasks (
    id                TEXT PRIMARY KEY,
    workspace_id      TEXT NOT NULL REFERENCES workspaces(id),
    requester_id      TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    target_id         TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    session_id        TEXT UNIQUE,        -- bridge session ID (ts_xxx)
    skill             TEXT,
    input_json        TEXT NOT NULL,
    output_json       TEXT,
    status            TEXT NOT NULL DEFAULT 'pending',
    -- pending → accepted → running → completed/failed/cancelled
    mode              TEXT NOT NULL DEFAULT 'async',
    failure_reason    TEXT,
    timeout_seconds   INTEGER DEFAULT 300,
    delegation_chain  TEXT NOT NULL DEFAULT '[]',
    epoch             INTEGER NOT NULL DEFAULT 0,
    total_cost_usd    REAL,
    num_turns         INTEGER DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    accepted_at       TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ
);

CREATE INDEX idx_agent_tasks_workspace ON agent_tasks(workspace_id);
CREATE INDEX idx_agent_tasks_requester ON agent_tasks(requester_id);
CREATE INDEX idx_agent_tasks_target_status ON agent_tasks(target_id, status);
CREATE INDEX idx_agent_tasks_session ON agent_tasks(session_id);
CREATE INDEX idx_agent_tasks_cleanup ON agent_tasks(status, completed_at);

-- Task transcript storage (JSONL sidecar files, not in DB)
-- Path: /data/transcripts/{workspace_id}/{task_id}.jsonl
```

### API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/agent/tasks` | Create task (with optional `stream: true` for SSE) |
| GET | `/api/agent/tasks/{task_id}` | Get task status and result |
| GET | `/api/agent/tasks` | List requester's tasks |
| DELETE | `/api/agent/tasks/{task_id}` | Cancel running task (sends interrupt) |
| GET | `/api/agent/tasks/{task_id}/stream` | SSE stream of task messages (for requester) |

The `/stream` endpoint is new — it allows the **requester** to observe task execution in real-time:

```
GET /api/agent/tasks/{task_id}/stream
Accept: text/event-stream

data: {"type":"system","subtype":"task_started","task_id":"t1"}

data: {"type":"assistant","task_id":"t1","payload":{"message":{"content":[...]}}}

data: {"type":"system","subtype":"task_progress","task_id":"t1","payload":{"tool_count":3,"token_count":1500}}

data: {"type":"result","subtype":"success","task_id":"t1","payload":{"result":"Found 3 bugs..."}}
```

### Task Worker (Agent Side)

The `TaskWorker` runs inside the agent process (both local `agentserver-agent` and cloud `agentserver-task-runner`). It receives tasks via the bridge and executes them using the Go Agent SDK.

```go
// internal/agent/task_worker.go

type TaskWorker struct {
    workdir    string
    env        map[string]string
    maxTasks   int              // max concurrent tasks
    activeTasks sync.Map        // taskID → *activeTask
}

type activeTask struct {
    taskID  string
    stream  *agentsdk.Stream
    bridge  BridgeTransport
    cancel  context.CancelFunc
}

func (w *TaskWorker) HandleTask(ctx context.Context, bridge BridgeTransport, task *TaskRequest) {
    // Register task
    ctx, cancel := context.WithTimeout(ctx, time.Duration(task.TimeoutSeconds)*time.Second)
    defer cancel()

    opts := []agentsdk.QueryOption{
        agentsdk.WithCwd(w.workdir),
        agentsdk.WithPermissionMode(agentsdk.PermissionBypassAll),
        agentsdk.WithAllowDangerouslySkipPermissions(),
        agentsdk.WithMaxTurns(task.MaxTurns),
        agentsdk.WithMaxBudgetUSD(task.MaxBudgetUSD),
        agentsdk.WithSystemPrompt(task.SystemContext()),
        agentsdk.WithHooks(map[agentsdk.HookEvent][]agentsdk.HookMatcher{
            agentsdk.HookPreToolUse: {{
                Hooks: []agentsdk.HookCallback{
                    // Forward permission checks to requester via bridge control protocol
                    w.makePermissionForwarder(bridge),
                },
            }},
        }),
    }

    stream := agentsdk.Query(ctx, task.BuildPrompt(), opts...)
    defer stream.Close()

    at := &activeTask{taskID: task.ID, stream: stream, bridge: bridge, cancel: cancel}
    w.activeTasks.Store(task.ID, at)
    defer w.activeTasks.Delete(task.ID)

    // Stream all messages back to requester via bridge
    for stream.Next() {
        msg := stream.Current()
        bridgeMsg := &TaskMessage{
            Type:      msg.Type,
            Subtype:   msg.Subtype,
            TaskID:    task.ID,
            UUID:      uuid.New().String(),
            Payload:   msg.Raw,
            Timestamp: time.Now(),
        }
        bridge.Write(bridgeMsg)
    }

    // Send final result
    if result, _ := stream.Result(); result != nil {
        bridge.Write(&TaskMessage{
            Type:    "result",
            Subtype: result.Subtype,
            TaskID:  task.ID,
            UUID:    uuid.New().String(),
            Payload: mustMarshal(result),
        })
    }
}
```

### Cloud Agent: Bridge Endpoints

The `agentserver-task-runner` binary in cloud containers exposes bridge endpoints:

```go
// cmd/agentserver-task-runner/main.go

// Bridge endpoints (served on :3010 inside container)
mux.HandleFunc("POST /bridge/sessions", handleCreateSession)     // register worker
mux.HandleFunc("GET  /bridge/events/stream", handleSSEStream)    // SSE read channel
mux.HandleFunc("POST /bridge/events", handleWriteEvents)         // batch write channel
mux.HandleFunc("PUT  /bridge/worker", handleWorkerHeartbeat)     // heartbeat + state
```

### Permission Forwarding

When a task executor encounters a tool that requires permission, the control protocol forwards the prompt to the requester (adopted from Claude Code's bridge permission model):

```
Worker: claude CLI needs "Bash(rm -rf /tmp)" approval
  ↓
TaskWorker hook intercepts PreToolUse
  ↓
Worker sends control_request via bridge:
  { subtype: "can_use_tool", tool_name: "Bash", input: { command: "rm -rf /tmp" } }
  ↓
Server receives, forwards to requester via:
  - Requester's SSE stream (/api/agent/tasks/{id}/stream)
  - Or requester's yamux tunnel (if local)
  ↓
Requester (or its user) decides: allow/deny
  ↓
Server sends control_response back to worker
  ↓
Worker continues or aborts tool use
```

For automated agents (no human), the `WithPermissionMode(PermissionBypassAll)` option skips this entirely.

### Teammate Coordination

For multi-agent workflows where agents need to communicate peer-to-peer (not just requester→target):

```go
// internal/bridge/mailbox.go

// Mailbox enables inter-agent messaging within a workspace.
// Adopted from Claude Code's teammate mailbox system.
// Uses DB-backed storage (not filesystem) for cloud compatibility.

type MailboxMessage struct {
    ID        string    `json:"id"`
    From      string    `json:"from"`       // sender sandbox ID
    To        string    `json:"to"`         // recipient sandbox ID, or "*" for broadcast
    TeamID    string    `json:"team_id"`    // workspace-scoped team
    Text      string    `json:"text"`
    MsgType   string    `json:"msg_type"`   // "message" | "shutdown_request" | "plan_approval"
    CreatedAt time.Time `json:"created_at"`
    ReadAt    *time.Time `json:"read_at,omitempty"`
}
```

```sql
-- Part of 014_agent_tasks.sql

CREATE TABLE agent_mailbox (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    from_id      TEXT NOT NULL,
    to_id        TEXT NOT NULL,    -- sandbox ID or "*"
    team_id      TEXT NOT NULL DEFAULT '',
    text         TEXT NOT NULL,
    msg_type     TEXT NOT NULL DEFAULT 'message',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    read_at      TIMESTAMPTZ
);

CREATE INDEX idx_agent_mailbox_recipient ON agent_mailbox(to_id, read_at);
CREATE INDEX idx_agent_mailbox_team ON agent_mailbox(team_id, created_at);
```

API for mailbox (injected as MCP tools via Phase 4):

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/agent/mailbox/send` | Send message to agent or broadcast |
| GET | `/api/agent/mailbox/inbox` | Poll inbox (long-poll with 30s timeout) |

### Loop Detection, Overflow, Cleanup

Same as original design:
- **Loop detection**: `delegation_chain` JSON array, max depth 5
- **Overflow**: reject with `at_capacity` when at `max_concurrency`
- **Cleanup**: `TaskCleanupWorker` deletes completed tasks after 7 days; transcript JSONL files cleaned alongside

### Files

| Action | File | Description |
|--------|------|-------------|
| Create | `internal/bridge/transport.go` | BridgeTransport interface + TaskMessage |
| Create | `internal/bridge/yamux_bridge.go` | YamuxBridge implementation |
| Create | `internal/bridge/sse_bridge.go` | SSEBridge implementation |
| Create | `internal/bridge/dedup.go` | BoundedUUIDSet |
| Create | `internal/bridge/epoch.go` | EpochManager |
| Create | `internal/bridge/control.go` | ControlRequest/Response types |
| Create | `internal/bridge/session.go` | TaskSession lifecycle |
| Create | `internal/bridge/mailbox.go` | Inter-agent mailbox |
| Create | `internal/db/migrations/014_agent_tasks.sql` | Tasks + mailbox schema |
| Create | `internal/db/agent_tasks.go` | Task CRUD |
| Create | `internal/db/agent_mailbox.go` | Mailbox CRUD |
| Create | `internal/server/agent_tasks.go` | Task API handlers + SSE stream |
| Create | `internal/server/agent_tasks_test.go` | Tests |
| Create | `internal/server/task_cleanup.go` | TaskCleanupWorker |
| Create | `internal/agent/task_worker.go` | TaskWorker using Go Agent SDK |
| Create | `cmd/agentserver-task-runner/main.go` | Cloud bridge endpoint + task worker |
| Modify | `internal/tunnel/stream.go` | Add StreamTypeBridge |
| Modify | `internal/agent/client.go` | Accept bridge streams, start TaskWorker |
| Modify | `internal/sandboxproxy/tunnel.go` | Open bridge streams for tasks |
| Modify | `internal/server/server.go` | Wire task routes |
| Modify | `cmd/serve.go` | Start/stop TaskCleanupWorker |
| Modify | `Dockerfile` | Include agentserver-task-runner |

---

## Phase 4: MCP Integration + Tool Injection

### agentserver-mcp-bridge

A standalone Go binary that acts as a stdio MCP server. Claude Code connects to it via MCP configuration, and it proxies `discover_agents`, `delegate_task`, `check_task`, and `send_message` tools to the agentserver HTTP API.

```
cmd/agentserver-mcp-bridge/
├── main.go                 // stdio server, JSON-RPC 2.0
internal/mcpbridge/
├── bridge.go               // MCP server implementation
└── tools.go                // tool handlers
```

**Configuration in Claude Code** (via MCP server config):

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

### inject_tools Tunnel Frame

Server sends `inject_tools` control frame to local agents on tunnel connect and topology changes:

```json
{
  "type": "inject_tools",
  "tools": ["discover_agents", "delegate_task", "check_task", "send_message"],
  "api_base_url": "https://agentserver.example.com/api/agent",
  "auth_token": "<proxy_token>"
}
```

Agent-side: starts local `agentserver-mcp-bridge` process, writes MCP config to `.claude/settings.json`.

### Injected MCP Tools

| Tool | Description |
|------|-------------|
| `discover_agents` | Find agents by skill, tags, type, status |
| `delegate_task` | Create task (sync/async) with real-time streaming |
| `check_task` | Get task status and result |
| `send_message` | Send message to another agent (mailbox) |
| `read_inbox` | Read messages from inbox |

The `delegate_task` tool in async mode returns a task ID immediately. The AI model can then:
1. Call `check_task` periodically to poll status
2. Or use Claude Code's background agent pattern to monitor the SSE stream

### Files

| Action | File | Description |
|--------|------|-------------|
| Create | `cmd/agentserver-mcp-bridge/main.go` | MCP bridge binary |
| Create | `internal/mcpbridge/bridge.go` | MCP server implementation |
| Create | `internal/mcpbridge/tools.go` | Tool handlers (5 tools) |
| Modify | `internal/agent/client.go` | Handle inject_tools, start bridge |
| Modify | `internal/sandboxproxy/tunnel.go` | Send inject_tools on connect |
| Modify | `Dockerfile` | Include agentserver-mcp-bridge |

---

## Phase 5: Security + Observability

### JWT Fast-Path (Cloud-to-Cloud Direct Bridge)

For bridge connections between cloud agents in the same K8s namespace, skip the server relay:

1. Requester requests direct bridge to target
2. Server issues short-lived JWT (5min TTL):
   ```json
   {
     "requester_id": "sandbox-a",
     "target_id": "sandbox-b",
     "workspace_id": "ws-123",
     "task_id": "task-123",
     "exp": 1711382400
   }
   ```
3. Server returns target pod IP + bridge port + JWT
4. Requester establishes SSEBridge directly to target pod
5. Target validates JWT, creates TaskSession

### Audit Logging

```sql
-- Migration: 015_agent_interactions.sql
CREATE TABLE agent_interactions (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL,
    requester_id     TEXT NOT NULL,
    target_id        TEXT NOT NULL,
    interaction_type TEXT NOT NULL,
    detail_json      TEXT,
    status           TEXT NOT NULL,
    duration_ms      INTEGER,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_agent_interactions_workspace_time ON agent_interactions(workspace_id, created_at);

ALTER TABLE workspaces ADD COLUMN delegation_mode TEXT NOT NULL DEFAULT 'auto';
```

### Rate Limiting & Delegation Mode

- Per-agent: 10 tasks/min, per-workspace: 50 tasks/min
- `delegation_mode`: `auto` (immediate) or `approval` (human-in-the-loop)

### Dashboard API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/workspaces/{wid}/agents` | List agents with cards |
| GET | `/api/workspaces/{wid}/agent-tasks` | Task history + live status |
| GET | `/api/workspaces/{wid}/agent-tasks/{id}/stream` | SSE stream (for dashboard UI) |
| GET | `/api/workspaces/{wid}/agent-interactions` | Audit log |

### Files

| Action | File | Description |
|--------|------|-------------|
| Create | `internal/db/migrations/015_agent_interactions.sql` | Audit + delegation mode |
| Create | `internal/server/agent_jwt.go` | JWT issuance + validation |
| Create | `internal/server/audit.go` | Audit middleware |
| Create | `internal/db/agent_interactions.go` | Audit CRUD |
| Modify | `internal/server/agent_tasks.go` | Rate limiting, approval mode |

---

## End-to-End Example (with Bridge Transport)

**Scenario**: User tells local Claude Code to review Go code. A cloud agent specializes in Go.

```
1. User → Local Claude Code: "Review the Go code in /src/server"

2. AI model calls discover_agents({ skill: "code-review", tags: ["go"] })
   → MCP bridge → GET /api/agent/discovery/agents

3. Server returns: [{ agent_id: "go-expert", status: "available", ... }]

4. AI model calls delegate_task({
     target_id: "go-expert", skill: "code-review",
     input: { data_access: "git", git_url: "...", path: "src/server" },
     mode: "async"
   })
   → MCP bridge → POST /api/agent/tasks

5. Server creates TaskSession + establishes SSEBridge to cloud agent:
   - POST http://{pod_ip}:3010/bridge/sessions → epoch=1, worker_token
   - Open SSE read stream + HTTP POST write channel
   - Send task prompt via bridge

6. Cloud agent's TaskWorker receives prompt, starts Go Agent SDK:
   agentsdk.Query(ctx, prompt, WithCwd(...), ...)

7. REAL-TIME STREAMING (not polling!):
   Worker → Bridge → Server → Requester's SSE stream:
   - assistant messages as Claude generates them
   - tool_progress as tools execute
   - permission_request if tool needs approval

8. Task completes:
   Worker sends result → Server updates DB → SSE event to requester

9. AI model sees task_notification, calls check_task({ task_id: "..." })
   → Gets full result with code review findings

10. AI presents to user: "The Go Expert found 3 issues: ..."
```

**Key difference from original design**: Steps 6-8 are real-time streaming, not polling. The requester can observe the task executing live, interrupt it, or forward permission decisions — all via the bridge transport.

---

## Cross-Cutting Concerns

### Authentication Summary

| Operation | Auth Method |
|-----------|-------------|
| Discovery | proxy_token (AgentAuthMiddleware) |
| Card registration | proxy_token |
| Task creation | proxy_token |
| Bridge transport (local) | tunnel_token (yamux) |
| Bridge transport (cloud) | worker_token (per-session JWT) |
| Direct fast-path (K8s) | Short-lived JWT |
| Dashboard API | Cookie auth |
| Mailbox | proxy_token |

### Data Sharing Between Agents

Same as original design — shared volumes (cloud), embedded payload, git-based, or MCP proxy callback.

### Error Handling

- Agent offline during task → bridge transport error → task `failed` with `agent_offline`
- Task timeout → context deadline → task `failed` with `timeout`
- Bridge reconnect failure → task `failed` with `bridge_disconnected`
- Epoch conflict → old worker ejected → new worker registered
- Delegation loop → rejected with `delegation_loop_detected`
- Agent at capacity → rejected with `at_capacity`

### Data Retention

- Agent cards: retained while sandbox exists
- Agent tasks: 7-day retention after completion
- Task transcripts (JSONL): cleaned alongside tasks
- Mailbox messages: 24-hour retention after read
- Agent interactions: permanent audit trail
