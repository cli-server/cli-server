# Stateless Claude Code Architecture Design

**Date:** 2026-04-15  
**Status:** Draft  
**Author:** mryao + Claude

## 1. Overview

### 1.1 Problem Statement

The current agentserver architecture requires each agent to maintain its own Claude Code (CC) instance. This leads to:

- **Resource waste**: Each CC process consumes memory/CPU even when idle
- **No horizontal scaling**: CC processes are stateful, tied to specific sessions
- **Complex multi-agent orchestration**: N agents = N independent CC instances, hard to coordinate

### 1.2 Design Goal

Introduce a **stateless Claude Code** architecture:

1. **One logical CC** — a single centralized reasoning service (stateless, horizontally scalable)
2. **Context externalized** — session history stored as append-only event log in PostgreSQL, loaded on demand
3. **Tool execution externalized** — CC emits tool call intents, routed to remote executors (sandboxes / local agents) via MCP

### 1.3 Key Insights

**Insight 1: Tool externalization via MCP.** Claude Code supports `--tools` to selectively enable built-in tools, combined with `--mcp-config` to load a custom MCP server. We use `--tools "WebSearch,WebFetch"` to preserve CC's native web capabilities (no executor needed), while all filesystem/execution tools (Bash, Read, Edit, etc.) are disabled and replaced by our custom Tool Router MCP Server that routes to remote executors. MCP tools use `remote_` prefix (e.g., `remote_bash`, `remote_read`) to avoid name collision with CC's internal deny rules — `--tools` deny rules filter by tool name, and allow rules cannot override deny rules.

**Insight 2: Stateless execution via bridge mode.** Claude Code's `--sdk-url` flag connects CC to an external bridge server via SSE. In this mode, CC holds **zero state** — conversation history is loaded from the bridge server via SSE replay, and results are written back via HTTP POST. CC processes are fully disposable: if one dies, a new process reconnects to the same bridge session and resumes from where the last one left off. The epoch mechanism ensures consistency.

```bash
claude --print \
       --sdk-url http://cc-broker:8080/v1/sessions/{session_id} \
       --tools "WebSearch,WebFetch" \
       --mcp-config '{"mcpServers":{"tool-router":{"type":"http","url":"..."}}}' \
       --permission-mode bypassPermissions \
       --dangerously-skip-permissions \
       --no-session-persistence
```

## 2. Architecture

### 2.1 Service Topology

```
用户(微信/Web/API)
  │
  ▼
imbridge ──► agentserver ──► cc-broker ──────────► sandboxproxy
             (用户业务)  HTTP (推理编排)     HTTP   (连接/执行)
                │               │                    │    │
                │               │                 tunnel  HTTP
                │               │                    │    │
                │               │                    ▼    ▼
                │               │                Local   Sandbox
                │               │                Agent   (K8s/Docker)
                │               │                  │
                │               │                  │ 注册/心跳
                │               │                  ▼
                │               │            executor-registry
                │               │            (executor 生命周期)
                │               │
                │          ┌────┴────┐
                │          │         │
                │     OpenViking   Tool Router
                │     REST client  MCP Server
                │     (download/   
                │      upload)     
                │          │
                │          │ HTTP
                │          ▼
                │      OpenViking
                │   (context 持久化)
                │          │
                │          ▼
                │    Storage Backend
                │    (S3 / KV / SQLite)
                │
            PostgreSQL
         (共享 event log)
```

### 2.2 Service Responsibilities

| Service | Responsibilities | Does NOT handle |
|---------|-----------------|-----------------|
| **agentserver** | User auth, workspace/session CRUD, IM inbound routing, event log persistence, bridge SSE to frontend | CC execution, executor connectivity |
| **cc-broker** | CC worker management, bridge API (context SSE + event persistence), Tool Router MCP Server, calling sandboxproxy for tool execution, OpenViking download/upload for workspace context | Tunnel management, business logic, executor lifecycle |
| **sandboxproxy** | Tunnel management (WebSocket), sandbox HTTP connectivity, unified tool execution API | Business logic, CC reasoning, executor registration |
| **executor-registry** | Executor registration (OAuth), heartbeat, capability storage, capability probe triggering | Tunnel management, tool execution |
| **OpenViking** | Persistent context storage (CLAUDE.md, Memory, Settings, Skills); serves FUSE client requests via HTTP; pluggable storage backend (S3/KV/SQLite) | CC execution, business logic |
| **imbridge** | IM platform long-polling (WeChat/Telegram/Matrix), message forwarding to agentserver, outbound replies | Session management, CC execution |

### 2.3 Inter-Service Communication

All inter-service communication uses **HTTP**. Streaming responses (cc-broker → agentserver) use **SSE** (Server-Sent Events), consistent with the existing bridge SSE pattern.

```
executor-registry ◄── local agent (register, heartbeat)
executor-registry ◄── agentserver (register sandbox, query executors)
executor-registry ◄── cc-broker (write back probe results, query executors)

sandboxproxy ◄── local agent (tunnel WebSocket)
sandboxproxy ◄── cc-broker (tool execution)
sandboxproxy ──► executor-registry (query executor info, validate tunnel token)

cc-broker ◄── agentserver (ProcessTurn via SSE)
cc-broker ──► sandboxproxy (tool execution)
cc-broker ──► executor-registry (query executors, write back capabilities)
cc-broker ──► OpenViking (REST API: download context before CC, upload changes after CC)

agentserver ──► cc-broker (ProcessTurn)
agentserver ──► executor-registry (register sandbox, query executors)
```

## 3. Message Ingestion Flow

### 3.1 Current Flow (NanoClaw-based)

```
WeChat user sends message
  → iLink API
  → imbridge long-poll receives message
  → POST http://{podIP}:3002/message  (direct to NanoClaw pod)
  → CC runs inside NanoClaw pod
  → NanoClaw calls POST /api/internal/nanoclaw/{id}/im/send
  → imbridge → WeChat reply
```

### 3.2 New Flow (Stateless CC)

```
WeChat user sends message
  → iLink API
  → imbridge long-poll receives message
  → POST /api/workspaces/{wid}/im/inbound  (to agentserver)
  → agentserver resolves/creates session
  → HTTP POST to cc-broker (SSE streaming response)
  → cc-broker assigns CC worker, processes turn
  → Tool calls routed via sandboxproxy to executors
  → Streaming results back to agentserver
  → agentserver persists events, replies via imbridge
  → imbridge → WeChat reply
```

### 3.3 imbridge Change

The only change in imbridge: `forwardToNanoClaw()` becomes `forwardToAgentserver()`.

```go
// Before: direct to pod
func (b *Bridge) forwardToNanoClaw(ctx context.Context, binding BridgeBinding, msg InboundMessage) error {
    podIP := binding.SandboxPodIP
    return httpPost(fmt.Sprintf("http://%s:3002/message", podIP), msg)
}

// After: to agentserver unified inbound endpoint
func (b *Bridge) forwardMessage(ctx context.Context, binding BridgeBinding, msg InboundMessage) error {
    return httpPost(
        fmt.Sprintf("%s/api/workspaces/%s/im/inbound", b.agentserverURL, binding.WorkspaceID),
        msg,
    )
}
```

### 3.4 agentserver IM Inbound Endpoint

```go
// POST /api/workspaces/{wid}/im/inbound
func (s *Server) handleIMInbound(w http.ResponseWriter, r *http.Request) {
    workspaceID := chi.URLParam(r, "wid")
    var msg IMInboundMessage
    json.NewDecoder(r.Body).Decode(&msg)

    // 1. Resolve session by chat_jid (same jid → same session)
    session, err := s.resolveIMSession(r.Context(), workspaceID, msg)

    // 2. Query available executors in workspace
    executors, _ := s.executorRegistryClient.ListExecutors(r.Context(), workspaceID)

    // 4. Async: call cc-broker (don't block HTTP response)
    //    Use background context — r.Context() is cancelled after 202 response
    bgCtx := context.Background()
    go s.processWithCCBroker(bgCtx, session, executors, msg)

    // 5. Return 202 immediately
    w.WriteHeader(http.StatusAccepted)
}

func (s *Server) resolveIMSession(ctx context.Context, workspaceID string, msg IMInboundMessage) (*AgentSession, error) {
    session, err := s.db.GetSessionByExternalID(ctx, workspaceID, msg.ChatJID)
    if err == ErrNotFound {
        session, err = s.db.CreateAgentSession(ctx, CreateSessionParams{
            WorkspaceID: workspaceID,
            ExternalID:  msg.ChatJID,     // "user123@im.wechat"
            Title:       fmt.Sprintf("WeChat: %s", msg.SenderName),
            Source:      msg.Provider,
        })
    }
    return session, err
}
```

### 3.5 Async Processing and Reply

```go
func (s *Server) processWithCCBroker(ctx context.Context, session *AgentSession,
    executors []ExecutorInfo, msg IMInboundMessage) {

    // 1. Call cc-broker ProcessTurn (SSE stream)
    //    Note: history is NOT passed here — cc-broker loads it from DB
    //    and serves it to CC via its bridge SSE endpoint
    eventStream, err := s.ccBrokerClient.ProcessTurn(ctx, ProcessTurnRequest{
        SessionID:   session.ID,
        WorkspaceID: session.WorkspaceID,
        UserMessage: msg.Content,
        Executors:   executors,
    })

    // 2. Consume streaming events
    var finalResponse string
    for event := range eventStream {
        // Persist to event log
        s.db.InsertSessionEvent(ctx, session.ID, event)
        // Push to frontend via bridge SSE
        s.bridge.Broadcast(session.ID, event)
        // Extract final text response
        if event.EventType == "assistant_message" {
            finalResponse = extractText(event.Payload)
        }
    }

    // 3. Reply to WeChat via imbridge
    channel, _ := s.db.GetIMChannelByWorkspace(ctx, session.WorkspaceID, msg.Provider)
    provider := s.imProviders[msg.Provider]
    provider.Send(ctx, channel.Credentials(), msg.ChatJID, finalResponse, nil)
}
```

## 4. Context Management (Event Sourcing via Bridge)

### 4.1 Storage

All session messages are stored as append-only events in PostgreSQL, reusing the existing two-table structure:

```sql
-- User-visible conversation events
agent_session_events (
    id            BIGSERIAL,          -- global sequence number
    session_id    TEXT,                -- owning session
    event_id      TEXT UNIQUE,         -- dedup ID
    event_type    TEXT,                -- 'message' | 'metadata'
    source        TEXT,                -- 'user' | 'assistant' | 'system' | 'tool_result'
    epoch         INTEGER,            -- which worker wrote this
    payload       JSONB,              -- CC SerializedMessage (stored as-is)
    ephemeral     BOOLEAN,            -- transient messages (not persisted)
    created_at    TIMESTAMPTZ
)

-- Internal events (compaction, transcript)
agent_session_internal_events (
    id            BIGSERIAL,
    session_id    TEXT,
    event_type    TEXT,
    payload       JSONB,
    is_compaction BOOLEAN DEFAULT FALSE,  -- marks compaction boundaries
    created_at    TIMESTAMPTZ
)
```

### 4.2 Context Loading via Bridge SSE (No JSONL Materialization)

CC connects to cc-broker via `--sdk-url`. Context loading uses the bridge's native SSE replay mechanism — **no JSONL file materialization needed**:

```
CC worker starts with --sdk-url http://cc-broker:8080/v1/sessions/{id}
  │
  ├─ 1. POST .../bridge → attach as worker, get JWT + epoch
  │     (cc-broker atomically bumps epoch, invalidating any stale worker)
  │
  ├─ 2. GET .../worker/events/stream → SSE replay
  │     cc-broker reads from agent_session_events (by sequence order)
  │     streams each event as SSE to CC
  │     CC reconstructs conversation history in-memory
  │
  ├─ 3. CC processes current turn (reasoning + tool calls)
  │
  └─ 4. POST .../worker/events/batch → write results back
        cc-broker persists to agent_session_events
        cc-broker forwards to agentserver via SSE
```

This approach:
- Eliminates fragile JSONL serialization/deserialization
- Reuses the existing bridge API implementation (`internal/bridge/server.go`)
- Handles `parentUuid` chain reconstruction natively (CC does this internally from SSE events)
- Supports epoch-based consistency out of the box

### 4.3 Bridge API in cc-broker

cc-broker implements the same bridge API as the existing agentserver bridge (CCR v2 compatible):

```go
// cc-broker bridge endpoints (reuse existing bridge/server.go patterns)
r.Post("/v1/sessions/{id}/bridge",              handleBridgeAttach)    // worker attach, epoch bump
r.Get("/v1/sessions/{id}/worker/events/stream", handleEventSSE)        // SSE replay
r.Post("/v1/sessions/{id}/worker/events/batch", handleEventBatch)      // write results
r.Post("/v1/sessions/{id}/worker/heartbeat",    handleWorkerHeartbeat) // liveness
```

Event SSE replay with compaction optimization:

```go
func (b *BridgeHandler) handleEventSSE(w http.ResponseWriter, r *http.Request) {
    sessionID := chi.URLParam(r, "id")

    // Check for compaction boundary in internal events table
    boundary, _ := b.db.GetLatestCompaction(ctx, sessionID)
    // WHERE is_compaction = TRUE ORDER BY id DESC LIMIT 1

    var events []SessionEvent
    if boundary != nil {
        // Load compaction summary + events after boundary
        events, _ = b.db.GetSessionEventsAfter(ctx, sessionID, boundary.Sequence)
        // Prepend compaction summary
        events = append([]SessionEvent{boundary.AsSummaryEvent()}, events...)
    } else {
        events, _ = b.db.GetSessionEvents(ctx, sessionID,
            WithNonEphemeral(), WithOrderBySequence())
    }

    // Stream as SSE
    flusher := w.(http.Flusher)
    for _, evt := range events {
        fmt.Fprintf(w, "data: %s\n\n", evt.Payload)
        flusher.Flush()
    }
    // Keep connection open for new events...
}
```

### 4.4 Compaction

CC's built-in auto-compaction remains functional. Compaction data is stored in `agent_session_internal_events` (consistent with existing codebase):

1. CC triggers compaction (LLM summarizes old messages)
2. CC writes compaction event via `POST .../worker/events/batch`
3. cc-broker stores it in `agent_session_internal_events` with `is_compaction = TRUE`
4. Next SSE replay loads only compaction summary + subsequent events

## 5. Tool Router MCP Server

### 5.1 Architecture

The Tool Router MCP Server runs inside cc-broker. It exposes the same tool interfaces as CC's built-in tools, but routes actual execution to remote executors via sandboxproxy.

```
CC Worker
  │  MCP protocol (HTTP)
  ▼
Tool Router MCP Server (in cc-broker)
  │
  ├─ list_executors                → query executor-registry
  ├─ remote_bash(executor_id, ...) → POST sandboxproxy /api/execute
  ├─ remote_edit(executor_id, ...) → POST sandboxproxy /api/execute
  ├─ remote_read(executor_id, ...) → POST sandboxproxy /api/execute
  └─ ...
```

Note: MCP tools use `remote_` prefix to avoid collision with CC's built-in tool deny rules. When `--tools "WebSearch,WebFetch"` disables built-in tools like Bash/Read/Edit, CC's deny rules also filter MCP tools with the same names. The `remote_` prefix ensures MCP tools are not caught by these deny rules.

### 5.2 MCP Tool Definitions

**Discovery tool:**

```json
{
  "name": "list_executors",
  "description": "List all available executors in the current workspace with their capabilities, status, and environment. Call this to understand what execution environments are available before delegating tool calls.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "status_filter": {
        "type": "string",
        "enum": ["online", "all"],
        "default": "online"
      }
    }
  }
}
```

Example response:

```json
[
  {
    "executor_id": "sbx_abc123",
    "display_name": "GPU Python Sandbox",
    "executor_type": "sandbox",
    "status": "online",
    "tools": ["Bash", "Read", "Edit", "Write", "Glob", "Grep"],
    "environment": {"python": "3.11", "cuda": "12.1", "gpu": "A100"},
    "description": "GPU-enabled sandbox with PyTorch, TensorFlow pre-installed",
    "working_dir": "/workspace"
  },
  {
    "executor_id": "agt_def456",
    "display_name": "Dev Machine Agent",
    "executor_type": "local_agent",
    "status": "online",
    "tools": ["Bash", "Read", "Edit", "Write", "Glob", "Grep"],
    "environment": {"go": "1.22", "node": "20", "git": "2.43"},
    "description": "Developer laptop with access to private git repos and company VPN",
    "working_dir": "/home/dev/projects/myapp"
  }
]
```

**Execution tools (each with `executor_id` parameter):**

All standard CC tools are exposed with a `remote_` prefix and an additional `executor_id` parameter. The prefix avoids collision with CC's internal deny rules. Example:

```json
{
  "name": "remote_bash",
  "description": "Execute a shell command on the specified executor.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "executor_id": {
        "type": "string",
        "description": "Target executor ID. Use list_executors to see available options. Required."
      },
      "command": { "type": "string", "description": "The command to execute" },
      "timeout": { "type": "number", "description": "Timeout in milliseconds" },
      "description": { "type": "string", "description": "What this command does" }
    },
    "required": ["executor_id", "command"]
  }
}
```

Full tool set exposed by the MCP server:

| Tool | Key Parameters | Description |
|------|---------------|-------------|
| `list_executors` | `status_filter` | Discover workspace executors |
| `remote_bash` | `executor_id`, `command`, `timeout` | Execute shell command |
| `remote_read` | `executor_id`, `file_path`, `offset`, `limit` | Read file (supports images/PDFs, returns base64) |
| `remote_edit` | `executor_id`, `file_path`, `old_string`, `new_string` | Edit file |
| `remote_write` | `executor_id`, `file_path`, `content` | Write file |
| `remote_glob` | `executor_id`, `pattern`, `path` | File pattern search |
| `remote_grep` | `executor_id`, `pattern`, `path`, `glob` | Content search |
| `remote_ls` | `executor_id`, `path` | List directory |
| `workspace_write` | `path`, `content` | Write file to workspace context (skills, instructions, memory) |
| `workspace_read` | `path` | Read file from workspace context |
| `workspace_ls` | `path` | List workspace context directory |
| `create_scheduled_task` | `cron`, `prompt`, `recurring` | Create scheduled task on agentserver |
| `list_scheduled_tasks` | | List workspace scheduled tasks |
| `cancel_scheduled_task` | `task_id` | Cancel a scheduled task |

### 5.3 Tool Routing Classification

Not all MCP tools route to sandboxproxy. The Tool Router classifies tools by destination:

| Tool Category | Destination | Examples |
|---------------|-------------|----------|
| **Executor tools** (`remote_*`) | sandboxproxy → executor | `remote_bash`, `remote_read`, `remote_edit`, `remote_write`, `remote_glob`, `remote_grep`, `remote_ls` |
| **Workspace tools** | cc-broker local filesystem | `workspace_write`, `workspace_read`, `workspace_ls` |
| **Discovery tools** | executor-registry | `list_executors` |
| **Scheduling tools** | agentserver | `create_scheduled_task`, `list_scheduled_tasks`, `cancel_scheduled_task` |
| **User interaction** | cc-broker → agentserver → IM | `AskUserQuestion` (Section 16.1) |

### 5.4 Permission Enforcement

```go
func (r *ToolRouter) HandleToolCall(ctx context.Context, req MCPToolCallRequest) (MCPToolResult, error) {
    sessionID := req.Meta["session_id"]
    session, _ := r.db.GetAgentSession(ctx, sessionID)
    workspaceID := session.WorkspaceID

    executorID := req.Arguments["executor_id"]

    // 1. Permission check: executor must belong to same workspace
    executor, err := r.executorRegistry.GetExecutor(ctx, executorID)
    if err != nil || executor.WorkspaceID != workspaceID {
        return errorResult("Permission denied: executor %s not in workspace %s",
            executorID, workspaceID), nil
    }

    // 2. Capability check: executor must support this tool
    if !executor.SupportsTool(req.Name) {
        return errorResult("Executor %s does not support tool %s. Supported: %v",
            executorID, req.Name, executor.Tools), nil
    }

    // 3. Status check
    if executor.Status != "online" {
        return errorResult("Executor %s is %s", executorID, executor.Status), nil
    }

    // 4. Dispatch via sandboxproxy (strip executor_id from tool args)
    toolArgs := stripExecutorID(req.Arguments)
    result, err := r.sandboxProxy.Execute(ctx, ExecuteRequest{
        ExecutorID: executorID,
        Tool:       req.Name,
        Arguments:  toolArgs,
    })

    return toMCPResult(result), err
}
```

### 5.4 Example CC Usage Flow

```
User: "帮我把 dev 机器上的代码部署到测试环境"

CC Turn 1: list_executors
  ← [Dev Machine Agent (go, node, git), Test Sandbox (docker, k8s)]

CC Turn 2: remote_read(executor_id="agt_dev", file_path="~/app/deploy.sh")
  → Tool Router → sandboxproxy → tunnel → dev machine
  ← file content

CC Turn 3: remote_bash(executor_id="sbx_test", command="bash deploy.sh")
  → Tool Router → sandboxproxy → HTTP → test sandbox pod
  ← deployment output

CC Turn 4: Reply "部署完成，以下是输出..."
```

## 6. CC Broker

### 6.1 External API (agentserver → cc-broker)

```
POST /api/turns
Content-Type: application/json

{
    "session_id": "cse_xxx",
    "workspace_id": "ws_xxx",
    "user_message": "帮我修复这个 bug",
    "executors": [ ... executor list ... ]
}

Response: Content-Type: text/event-stream

data: {"event_type":"assistant_message","payload":{...}}
data: {"event_type":"tool_use","payload":{...}}
data: {"event_type":"tool_result","payload":{...}}
data: {"event_type":"assistant_message","payload":{...}}
data: {"event_type":"done","payload":{...}}
```

Note: `history` is no longer passed in the request. cc-broker loads history from its own DB and serves it to CC via the bridge SSE endpoint (Section 4).

### 6.2 Internal API (CC worker → cc-broker bridge)

cc-broker also exposes the bridge API (Section 4.3) that CC workers connect to via `--sdk-url`. This is an internal API, not called by agentserver.

### 6.3 CC Worker Lifecycle

Each CC worker is a short-lived process spawned per turn. CC connects to cc-broker's bridge endpoint via `--sdk-url`, processes one turn (which may involve multiple tool calls), then exits.

```go
type CCWorker struct {
    ID        string
    Process   *exec.Cmd
    SessionID string
    Status    WorkerStatus // starting | running | done | dead
    StartedAt time.Time
}
```

### 6.4 Worker Startup

```go
func (b *CCBroker) spawnWorker(ctx context.Context, sessionID, workspaceID string) (*CCWorker, error) {
    // Generate dynamic MCP config with session_id for permission scoping
    mcpConfig := b.buildMCPConfig(sessionID, workspaceID)
    mcpConfigPath := writeTempMCPConfig(mcpConfig)

    bridgeURL := fmt.Sprintf("http://localhost:%d/v1/sessions/%s", b.bridgePort, sessionID)

    cmd := exec.CommandContext(ctx, "claude",
        "--sdk-url", bridgeURL,           // connect to cc-broker's bridge
        "--tools", "WebSearch,WebFetch",                     // disable all built-in tools
        "--mcp-config", mcpConfigPath,     // Tool Router MCP Server
        "--bare",                          // minimal init
    )
    cmd.Env = append(os.Environ(),
        "ANTHROPIC_API_KEY="+b.apiKey,
        "CLAUDE_CODE_SIMPLE=1",
    )
    cmd.Start()

    return &CCWorker{
        ID:        uuid.New().String(),
        Process:   cmd,
        SessionID: sessionID,
        Status:    WorkerStatusStarting,
    }, nil
}
```

### 6.5 Request Processing

```go
func (b *CCBroker) HandleTurn(ctx context.Context, req ProcessTurnRequest) (<-chan TurnEvent, error) {
    // 1. Inject user message into session event log
    //    (CC will pick it up via bridge SSE replay)
    b.db.InsertSessionEvent(ctx, req.SessionID, UserMessageEvent(req.UserMessage))

    // 2. Spawn CC worker connected to this session's bridge
    worker, err := b.spawnWorker(ctx, req.SessionID, req.WorkspaceID)
    if err != nil {
        return nil, err
    }

    // 3. Stream events as CC writes them back via bridge batch endpoint
    //    cc-broker's bridge handler captures these and forwards to the output channel
    events := b.bridge.Subscribe(req.SessionID)

    go func() {
        // Wait for CC process to exit
        worker.Process.Wait()
        worker.Status = WorkerStatusDone
        // Cleanup
        close(events)
    }()

    return events, nil
}
```

### 6.6 Per-Session Turn Serialization

To prevent race conditions when a user sends multiple messages rapidly (common in WeChat), cc-broker enforces **one active turn per session**:

```go
type TurnLock struct {
    mu    sync.Mutex
    locks map[string]chan struct{} // session_id → lock channel
}

func (t *TurnLock) Acquire(sessionID string) {
    t.mu.Lock()
    ch, exists := t.locks[sessionID]
    if !exists {
        ch = make(chan struct{}, 1)
        t.locks[sessionID] = ch
    }
    t.mu.Unlock()
    ch <- struct{}{} // blocks if another turn is active
}

func (t *TurnLock) Release(sessionID string) {
    t.mu.Lock()
    if ch, exists := t.locks[sessionID]; exists {
        <-ch
    }
    t.mu.Unlock()
}
```

Usage in HandleTurn:

```go
func (b *CCBroker) HandleTurn(ctx context.Context, req ProcessTurnRequest) (<-chan TurnEvent, error) {
    b.turnLock.Acquire(req.SessionID)
    // ... process turn ...
    // Release in the cleanup goroutine after CC exits
}
```

### 6.7 Health Management

```go
func (b *CCBroker) healthLoop() {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        b.mu.Lock()
        for id, w := range b.activeWorkers {
            // Kill workers that exceed max duration (e.g., 30 minutes)
            if time.Since(w.StartedAt) > b.maxTurnDuration {
                w.Process.Kill()
                delete(b.activeWorkers, id)
            }
            // Kill workers with excessive memory
            if getProcessMemoryMB(w.Process.Process.Pid) > b.maxMemoryMB {
                w.Process.Kill()
                delete(b.activeWorkers, id)
            }
        }
        b.mu.Unlock()
    }
}
```

### 6.8 Horizontal Scaling

Each cc-broker instance spawns CC workers on demand. Requests can be routed to any cc-broker instance since all state is in PostgreSQL:

```
cc-broker-1  ←─┐
cc-broker-2  ←─┤── Load Balancer
cc-broker-3  ←─┘
                  ↕
             PostgreSQL (shared event log + session state)
```

CC workers are ephemeral — spawned per turn, exit after completion. No process pool or sticky sessions needed.

## 7. Sandbox Proxy

### 7.1 API

```go
// POST /api/execute — Unified tool execution
type ExecuteRequest struct {
    ExecutorID string          `json:"executor_id"`
    Tool       string          `json:"tool"`
    Arguments  json.RawMessage `json:"arguments"`
    Timeout    time.Duration   `json:"timeout"`
}

type ExecuteResponse struct {
    Output   string `json:"output"`
    ExitCode int    `json:"exit_code"`
}

// GET /api/executors/{id}/status — Check executor connectivity
type ExecutorStatus struct {
    ExecutorID string `json:"executor_id"`
    Connected  bool   `json:"connected"`
}
```

### 7.2 Routing Logic

sandboxproxy determines how to reach each executor:

```go
func (p *SandboxProxy) Execute(ctx context.Context, req ExecuteRequest) (ExecuteResponse, error) {
    executor, _ := p.executorRegistry.GetExecutor(ctx, req.ExecutorID)

    switch executor.Type {
    case "sandbox":
        // Direct HTTP to sandbox pod
        return p.execViaPodHTTP(ctx, executor.PodIP, req)

    case "local_agent":
        // Push via tunnel
        tunnel, ok := p.tunnelRegistry.Get(req.ExecutorID)
        if !ok {
            return ExecuteResponse{}, fmt.Errorf("executor %s not connected", req.ExecutorID)
        }
        return p.execViaTunnel(ctx, tunnel, req)
    }
}
```

### 7.3 Tunnel Management

Local agents establish WebSocket tunnels to sandboxproxy (moved from agentserver):

```go
// WebSocket endpoint for local agent tunnel
// WS /api/tunnel/{executor_id}?token={tunnel_token}
func (p *SandboxProxy) handleTunnel(w http.ResponseWriter, r *http.Request) {
    executorID := chi.URLParam(r, "executor_id")
    token := r.URL.Query().Get("token")

    // Validate tunnel token via executor-registry
    valid, _ := p.executorRegistry.ValidateTunnelToken(r.Context(), executorID, token)
    if !valid {
        http.Error(w, "unauthorized", 401)
        return
    }

    // Upgrade to WebSocket + yamux
    conn, _ := upgrader.Upgrade(w, r, nil)
    session, _ := yamux.Server(conn, yamux.DefaultConfig())
    p.tunnelRegistry.Register(executorID, &Tunnel{Session: session})
}
```

### 7.4 Tool Executor Agent

Both sandboxes and local agents run a lightweight **Tool Executor Agent** — a minimal HTTP server that receives tool call requests and executes them locally:

```go
// Runs inside sandbox pod or on local agent machine
func handleToolExecute(w http.ResponseWriter, r *http.Request) {
    var req ToolCallRequest
    json.NewDecoder(r.Body).Decode(&req)

    var result ToolCallResult
    switch req.Tool {
    case "Bash":
        result = executeBash(req.Arguments)
    case "Read":
        result = executeRead(req.Arguments)
    case "Edit":
        result = executeEdit(req.Arguments)
    case "Write":
        result = executeWrite(req.Arguments)
    case "Glob":
        result = executeGlob(req.Arguments)
    case "Grep":
        result = executeGrep(req.Arguments)
    case "LS":
        result = executeLS(req.Arguments)
    }

    json.NewEncoder(w).Encode(result)
}
```

## 8. Executor Registry

### 8.1 API

```go
// Local agent registration (OAuth Device Flow)
// POST /api/executors/register
type RegisterRequest struct {
    Token       string `json:"token"`
    Name        string `json:"name"`
    WorkspaceID string `json:"workspace_id"`
}
type RegisterResponse struct {
    ExecutorID    string `json:"executor_id"`
    TunnelToken   string `json:"tunnel_token"`   // for connecting to sandboxproxy
    RegistryToken string `json:"registry_token"` // for subsequent heartbeats
}

// Heartbeat
// PUT /api/executors/{id}/heartbeat
type HeartbeatRequest struct {
    Status       string            `json:"status"`
    SystemInfo   map[string]string `json:"system_info"`
    Capabilities *ExecutorCapability `json:"capabilities,omitempty"`
}

// Sandbox registration (called by agentserver on sandbox creation)
// POST /api/executors/sandbox
type RegisterSandboxRequest struct {
    SandboxID    string             `json:"sandbox_id"`
    WorkspaceID  string             `json:"workspace_id"`
    Name         string             `json:"name"`
    Capabilities ExecutorCapability `json:"capabilities"`
}

// Query executors (called by agentserver, cc-broker)
// GET /api/executors?workspace_id=xxx
// GET /api/executors/{id}

// Update capabilities (called by cc-broker after probe)
// PUT /api/executors/{id}/capabilities

// Validate tunnel token (called by sandboxproxy)
// POST /api/executors/validate-tunnel-token
```

### 8.2 Executor Capability Model

```go
type ExecutorCapability struct {
    ExecutorID   string            `json:"executor_id"`
    DisplayName  string            `json:"display_name"`
    ExecutorType string            `json:"executor_type"` // "sandbox" | "local_agent"
    Status       string            `json:"status"`        // "online" | "busy" | "offline"
    Tools        []string          `json:"tools"`
    Environment  map[string]string `json:"environment"`
    Resources    ResourceInfo      `json:"resources"`
    Description  string            `json:"description"`
    WorkingDir   string            `json:"working_dir"`
    WorkspaceID  string            `json:"workspace_id"`
}

type ResourceInfo struct {
    CPUCores  int    `json:"cpu_cores"`
    MemoryGB  int    `json:"memory_gb"`
    DiskGB    int    `json:"disk_gb"`
    GPU       string `json:"gpu,omitempty"`
}
```

### 8.3 CC-Driven Capability Probing

When a new executor registers, executor-registry triggers a capability probe via cc-broker:

```go
func (s *Registry) handleRegister(w http.ResponseWriter, r *http.Request) {
    // ... registration logic ...
    executor, _ := s.db.CreateExecutor(ctx, ...)

    // Async: trigger capability probe via cc-broker
    go s.triggerCapabilityProbe(ctx, executor)
}

func (s *Registry) triggerCapabilityProbe(ctx context.Context, executor Executor) {
    // Call cc-broker to run a probe session
    s.ccBrokerClient.ProbeExecutor(ctx, ProbeExecutorRequest{
        ExecutorID:  executor.ID,
        WorkspaceID: executor.WorkspaceID,
    })
    // cc-broker will:
    // 1. Start a CC worker with the probe prompt
    // 2. CC runs commands on the executor via sandboxproxy
    // 3. CC outputs structured capability report
    // 4. cc-broker writes results back via PUT /api/executors/{id}/capabilities
}
```

Probe prompt used by cc-broker:

```
Probe executor {executor_id} to discover its capabilities.
Run commands to determine:
1. OS and architecture
2. Installed programming languages and versions
3. Available package managers
4. Hardware resources (CPU cores, memory, disk, GPU)
5. Network access capabilities
6. Installed development tools (git, docker, kubectl, etc.)
7. Any notable pre-installed frameworks or libraries

After probing, output a JSON block:
{
  "tools": ["Bash", "Read", "Edit", "Write", "Glob", "Grep"],
  "environment": {"key": "value", ...},
  "resources": {"cpu_cores": N, "memory_gb": N, "disk_gb": N, "gpu": "..."},
  "description": "Natural language description of this executor's capabilities"
}
```

User-declared capabilities (e.g., "has VPN access" — things CC can't probe) are configured via agent config file and merged with probe results, with user declarations taking priority.

### 8.4 Database

```sql
executors (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL,        -- 'local_agent' | 'sandbox'
    status          TEXT DEFAULT 'online',
    tunnel_token    TEXT,
    registry_token  TEXT,
    created_at      TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ
);

executor_capabilities (
    executor_id     TEXT PRIMARY KEY REFERENCES executors(id),
    tools           JSONB,                -- ["Bash","Read",...]
    environment     JSONB,                -- {"python":"3.11",...}
    resources       JSONB,                -- {"cpu_cores":4,...}
    description     TEXT,
    probed_at       TIMESTAMPTZ,          -- last probe time
    user_declared   JSONB                 -- user-configured overrides
);

executor_heartbeats (
    executor_id     TEXT REFERENCES executors(id),
    last_seen       TIMESTAMPTZ,
    system_info     JSONB
);
```

## 9. Local Agent Lifecycle

### 9.1 Startup Flow

```
agentserver-agent binary starts
  │
  ├─ 1. Register with executor-registry
  │     POST /api/executors/register (OAuth token)
  │     ← executor_id, tunnel_token, registry_token
  │
  ├─ 2. Establish tunnel to sandboxproxy
  │     WS /api/tunnel/{executor_id}?token={tunnel_token}
  │     (persistent connection, receives tool call requests)
  │
  ├─ 3. Start tool executor HTTP handler (on tunnel)
  │     Handles: Bash, Read, Edit, Write, Glob, Grep, LS
  │
  └─ 4. Periodic heartbeat to executor-registry
        PUT /api/executors/{id}/heartbeat (every 20s)
        Reports: status, basic system info
```

### 9.2 Agent Binary Changes

The `agentserver-agent` binary simplifies significantly:

| Before (current) | After (new) |
|-------------------|-------------|
| Register with agentserver | Register with executor-registry |
| Establish tunnel to agentserver | Establish tunnel to sandboxproxy |
| Poll for tasks | Receive tool calls via tunnel (push) |
| Start CC subprocess for each task | No CC — just execute tool calls locally |
| Run full agent loop | Simple tool executor |

## 10. End-to-End Example

### Scenario: User asks to train a model

```
① User sends WeChat message: "帮我训练一个模型，代码在 dev 机器的 ~/projects/ml 里"

② iLink API → imbridge polls → POST /api/workspaces/ws1/im/inbound → agentserver

③ agentserver:
   - Resolves session (by chat_jid)
   - Loads event history from DB (empty for new session)
   - Queries executor-registry: GET /api/executors?workspace_id=ws1
     Returns: [Dev Machine Agent (go, node, git), GPU Sandbox (A100, PyTorch)]
   - POST cc-broker /api/turns (SSE)

④ cc-broker:
   - Inserts user message into session event log
   - Acquires per-session turn lock
   - Spawns CC worker: claude --sdk-url http://cc-broker/v1/sessions/{id} --tools "" --mcp-config ... --bare
   - CC connects to bridge, replays history via SSE (empty for new session), starts reasoning

⑤ CC Agent Loop:
   Turn 1: list_executors
     → Tool Router → executor-registry
     ← [Dev Machine Agent, GPU Sandbox]

   Turn 2: remote_read(executor_id="agt_dev", file_path="~/projects/ml/train.py")
     → Tool Router → sandboxproxy → tunnel → Dev Machine
     ← file content (train.py)

   Turn 3: remote_read(executor_id="agt_dev", file_path="~/projects/ml/requirements.txt")
     → Tool Router → sandboxproxy → tunnel → Dev Machine
     ← file content (requirements.txt)

   Turn 4: remote_bash(executor_id="sbx_gpu", command="pip install -r /tmp/requirements.txt")
     → Tool Router → sandboxproxy → HTTP → GPU Sandbox
     ← pip install output

   Turn 5: remote_bash(executor_id="sbx_gpu", command="python /tmp/train.py --epochs 10")
     → Tool Router → sandboxproxy → HTTP → GPU Sandbox
     ← training output (loss, accuracy, etc.)

   Turn 6: CC replies "模型训练完成！最终 accuracy: 95.2%，模型已保存在 GPU sandbox 的 /workspace/model.pt"

⑥ cc-broker streams TurnEvents back to agentserver

⑦ agentserver:
   - Persists all events to agent_session_events
   - Extracts final text response
   - Calls imbridge provider.Send() → WeChat reply

⑧ User receives reply in WeChat
```

## 11. Key Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Agent Loop | Keep CC as-is | Reuse CC's reasoning, compaction, system prompt — no reimplementation |
| CC invocation | `--sdk-url` bridge mode | CC holds zero state; context loaded via SSE replay from cc-broker's bridge API |
| Statelessness | Externalize context + tools | CC process is disposable, any worker handles any session |
| Context storage | Event Sourcing (PostgreSQL) | Reuse existing `agent_session_events`, append-only, replayable |
| Context loading | Bridge SSE replay | No JSONL materialization; CC natively loads history from bridge SSE endpoint |
| Tool routing | MCP Server (`remote_` prefix) + `--tools "WebSearch,WebFetch"` | Preserve CC's native web tools; executor-bound tools via MCP with `remote_` prefix to avoid deny rule collision |
| Executor selection | CC (LLM) decides | LLM understands executor capabilities, makes intelligent routing |
| Capability discovery | CC-driven probing | Flexible, no agent binary updates needed |
| Service split | 5 services | agentserver, cc-broker, sandboxproxy, executor-registry, OpenViking; clean separation, independent scaling |
| Inter-service protocol | HTTP + SSE | Consistent with existing codebase, no new dependencies |
| Worker management | Per-turn process spawning | CC exits after each turn; `--bare` minimizes cold start; no stale process state |
| Turn serialization | Per-session lock | Prevents concurrent turn processing race conditions |
| Permission model | Workspace isolation | Session can only access executors in its own workspace |
| Side effect management | OpenViking download-run-upload | Download context before CC; CC reads/writes real filesystem; upload changes after CC exits |
| CLAUDE.md | Native discovery via downloaded files | Full feature support (@include, frontmatter, rules); synced from executor to OpenViking |
| Auto-Memory | Native read/write on local filesystem | Downloaded before CC; CC manages MEMORY.md natively; uploaded after CC exits |
| Settings/Permissions | Pre-configured in OpenViking, downloaded | bypassPermissions via CLI flags; shared across workers |
| Skills | Native discovery via downloaded files | Downloaded to `.claude/skills/`; CC discovers natively |
| `--bare` flag | Not needed | Downloaded files provide full `.claude/` structure; only truly incompatible features disabled via env vars |

## 12. New Components Summary

| Component | Location | Scope |
|-----------|----------|-------|
| cc-broker service | New Go service | Core: bridge API, CC worker management, Tool Router MCP |
| sandboxproxy service | New Go service (or extract from agentserver) | Tunnel management, unified tool execution API |
| executor-registry service | New Go service | Executor registration, heartbeat, capability storage |
| Tool Executor Agent | In sandbox images + agentserver-agent binary | Lightweight HTTP handler for tool execution |
| IM inbound endpoint | agentserver addition | `POST /api/workspaces/{wid}/im/inbound` |
| Schema migrations | agentserver DB | `sandbox_id` nullable, `external_id` column, `source` column |
| OpenViking integration | cc-broker | REST API download/upload per worker; workspace context tree management |
| Workspace context in OpenViking | OpenViking | `viking://workspace/{wid}/` tree: claude-home, project, skills, memory |

## 13. Schema Migrations

### 13.1 `agent_sessions.sandbox_id` → Nullable

The existing schema requires `sandbox_id NOT NULL`. In the new architecture, IM-originated sessions have no associated sandbox — reasoning is handled by cc-broker, not a per-agent CC.

```sql
-- Migration: make sandbox_id nullable
ALTER TABLE agent_sessions ALTER COLUMN sandbox_id DROP NOT NULL;

-- Add external_id for IM session resolution (chat_jid → session mapping)
ALTER TABLE agent_sessions ADD COLUMN external_id TEXT;
CREATE UNIQUE INDEX idx_agent_sessions_external_id
    ON agent_sessions(workspace_id, external_id) WHERE external_id IS NOT NULL;

-- Add source field to identify session origin
ALTER TABLE agent_sessions ADD COLUMN source TEXT DEFAULT 'agent';
-- source values: 'agent' (legacy), 'weixin', 'telegram', 'matrix', 'web', 'api'
```

### 13.2 Executor Tables (in executor-registry DB)

See Section 8.4 for full schema.

## 14. Migration Path

1. **Phase 1**: Schema migrations (sandbox_id nullable, external_id, executor tables, scheduled_tasks)
2. **Phase 2**: Build executor-registry + sandboxproxy as standalone services
3. **Phase 3**: Build cc-broker with bridge API, Tool Router MCP Server (with `remote_` prefixed tools), OpenViking download/upload integration, and worker management
4. **Phase 4**: Add IM inbound endpoint to agentserver, rewire imbridge, add scheduled task service
5. **Phase 5**: Modify agentserver-agent to register with executor-registry and connect tunnel to sandboxproxy
6. **Phase 6**: Integration testing — validate CC with `--sdk-url` + `--tools "WebSearch,WebFetch"` + MCP end-to-end
7. **Phase 7**: Deprecate per-agent CC instances, route all reasoning through cc-broker

## 15. Side Effect Management (OpenViking Download-Run-Upload)

CC produces side effects beyond conversation messages and tool calls: CLAUDE.md discovery, auto-memory read/write, settings, skills, plans, session transcripts, etc. In the stateless design, CC workers have no persistent local filesystem.

**Solution**: Use [OpenViking](/root/OpenViking) as the persistent context store, with a **download-run-upload** pattern:
1. **Before CC starts**: Download workspace context from OpenViking REST API to a local temp directory
2. **During CC run**: CC reads/writes files on the real local filesystem (zero overhead, full compatibility)
3. **After CC exits**: Diff changed files and upload back to OpenViking via REST API

This approach aligns with OpenViking's design philosophy: **reads via filesystem (or download), writes via API**. OpenViking's FUSE mount is designed for read-only access; writes go through the REST API which triggers semantic processing (vectorization, L0/L1 summary generation, relation graph updates).

### 15.1 Architecture

```
                    ① Download (before CC starts)
                    OpenViking REST API → local temp dir
                    GET viking://workspace/{wid}/claude-home/*
                    GET viking://workspace/{wid}/project/*

CC Worker process
  │
  │  normal file I/O on real local filesystem
  │  (zero overhead, full POSIX compatibility)
  ▼
Local temp directory
  ├── claude-config/              ← CLAUDE_CONFIG_DIR
  │   ├── settings.json
  │   ├── CLAUDE.md
  │   ├── skills/
  │   └── projects/ws_{wid}/memory/
  │       └── MEMORY.md           ← CC reads/writes natively
  │
  └── project/                    ← cwd
      ├── CLAUDE.md
      └── .claude/
          ├── rules/*.md
          ├── settings.json
          └── skills/

                    ② Upload (after CC exits)
                    Diff changed files → OpenViking REST API
                    POST /api/v1/content/write (for each modified file)
```

### 15.2 Why Download-Run-Upload

| Approach | Problem |
|----------|---------|
| `--append-system-prompt` injection | Loses CLAUDE.md features (@include, frontmatter, rules) |
| DB + MCP tools for memory | CC can't use native auto-memory; adds complexity |
| Physical shared filesystem (NFS/PVC) | Infrastructure dependency; not cloud-native |
| Ephemeral `$HOME` with no persistence | Memory, settings, plans all lost on worker exit |
| OpenViking FUSE mount | FUSE is read-only by design; writes don't persist (FUSE `release()` doesn't write back to storage — this is intentional, as OpenViking writes need semantic processing that can't fit in POSIX sync I/O) |
| **Download-Run-Upload** | **Real filesystem (zero overhead); full CC compatibility; writes go through OpenViking REST API (semantic processing intact)** |

### 15.3 Side Effect Classification

| Side Effect | Strategy | How It Works |
|-------------|----------|--------------|
| CLAUDE.md | **Download** | Downloaded from OpenViking to local temp dir; CC discovers natively with full feature support (@include, frontmatter, rules) |
| Auto-Memory | **Download + Upload** | Downloaded before CC starts; CC writes MEMORY.md natively to local filesystem; uploaded back to OpenViking after CC exits |
| Settings | **Download** | Pre-configured `settings.json` downloaded from OpenViking; read-only during CC run |
| Skills | **Download** | Downloaded to `{cwd}/.claude/skills/`; CC discovers and loads natively |
| Plans | **Download + Upload** | Downloaded before; CC writes plans locally; uploaded after |
| Session transcript | **Disable** | `--no-session-persistence`; bridge event log is source of truth |
| Cron/Scheduled tasks (built-in) | **Replace** | MCP `create_scheduled_task` → agentserver scheduler (Section 16.4) |
| Worktrees | **Remote** | No local git repo; remote worktree via `remote_bash` + `git worktree` on executor |
| Git internal ops | **Graceful fail** | No repo in cwd; all git goes through tool calls to executors |
| Telemetry | **Accept** | No env var to disable; CC may send telemetry to Anthropic (accepted risk) |
| File attribution | **Disable** | `CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING=1` |
| Keychain/OAuth | **Skip** | `ANTHROPIC_API_KEY` env var only |

### 15.4 OpenViking Workspace Layout

Each workspace has a persistent context tree in OpenViking:

```
viking://workspace/{workspace_id}/
  ├── claude-home/                    # → downloaded to CLAUDE_CONFIG_DIR
  │   ├── settings.json               # workspace config
  │   ├── CLAUDE.md                   # workspace-level global instructions
  │   ├── rules/
  │   │   └── *.md
  │   ├── skills/
  │   │   └── {skill-name}/
  │   │       └── skill.md
  │   └── projects/
  │       └── ws_{workspace_id}/
  │           └── memory/
  │               ├── MEMORY.md       # auto-memory index
  │               └── *.md            # memory entries
  │
  └── project/                        # → downloaded to cwd
      ├── CLAUDE.md                   # project instructions (synced from executor)
      ├── CLAUDE.local.md
      └── .claude/
          ├── CLAUDE.md
          ├── rules/*.md
          ├── settings.json
          └── skills/
              └── {skill-name}/
                  └── skill.md
```

### 15.5 Project Instructions Sync

CLAUDE.md and project rules are synced from executors to OpenViking. This happens:
- On workspace creation
- On executor capability probe (Section 8.3)
- On explicit user request (refresh)
- Periodically (configurable, e.g., hourly)

```go
func (b *CCBroker) syncProjectInstructions(ctx context.Context, workspaceID, executorID string) error {
    // Read CLAUDE.md from executor via sandboxproxy
    claudeMD, _ := b.sandboxProxy.Execute(ctx, ExecuteRequest{
        ExecutorID: executorID,
        Tool:       "Read",
        Arguments:  json.RawMessage(`{"file_path":"CLAUDE.md"}`),
    })

    // Write to OpenViking via REST API
    b.viking.Write(ctx, fmt.Sprintf("viking://workspace/%s/project/CLAUDE.md", workspaceID),
        claudeMD.Output)

    // Also sync .claude/rules/*.md, .claude/CLAUDE.md, etc.
    return nil
}
```

### 15.6 Workspace Context Tools

CC's built-in file tools (Write, Edit, Read) are disabled and replaced by `remote_*` MCP tools that operate on remote executors. However, CC also needs to read and write its own workspace context — skills, CLAUDE.md, memory entries, etc. These files live in the local temp directory (downloaded from OpenViking) and are NOT on any remote executor.

The Tool Router exposes workspace context tools for this purpose:

```json
[
  {
    "name": "workspace_write",
    "description": "Write a file to the workspace context. Use for creating/editing skills, instructions (CLAUDE.md), memory entries, and other workspace-level files. Changes persist across sessions.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "path": {
          "type": "string",
          "description": "Relative path within workspace context, e.g. 'skills/my-skill/skill.md', 'CLAUDE.md', 'rules/coding-standards.md'"
        },
        "content": { "type": "string", "description": "File content to write" }
      },
      "required": ["path", "content"]
    }
  },
  {
    "name": "workspace_read",
    "description": "Read a file from the workspace context (skills, instructions, memory).",
    "inputSchema": {
      "type": "object",
      "properties": {
        "path": { "type": "string", "description": "Relative path within workspace context" }
      },
      "required": ["path"]
    }
  },
  {
    "name": "workspace_ls",
    "description": "List files in a workspace context directory.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "path": { "type": "string", "default": "", "description": "Relative path, e.g. 'skills/' or 'rules/'" }
      }
    }
  }
]
```

These tools are handled directly by cc-broker (not routed to sandboxproxy or any executor). They read/write the CC worker's local temp directory:

```go
func (r *ToolRouter) HandleToolCall(ctx context.Context, req MCPToolCallRequest) (MCPToolResult, error) {
    switch req.Name {
    case "workspace_write":
        path := filepath.Join(r.claudeConfigDir, req.Arguments["path"])
        os.MkdirAll(filepath.Dir(path), 0755)
        os.WriteFile(path, []byte(req.Arguments["content"]), 0644)
        return MCPToolResult{Content: []ContentBlock{{Type: "text", Text: "Written successfully"}}}, nil

    case "workspace_read":
        path := filepath.Join(r.claudeConfigDir, req.Arguments["path"])
        content, err := os.ReadFile(path)
        // ...

    case "workspace_ls":
        path := filepath.Join(r.claudeConfigDir, req.Arguments["path"])
        entries, err := os.ReadDir(path)
        // ...
    }
}
```

After CC exits, changes are detected by the diff-upload step and persisted to OpenViking. The next CC worker for this workspace will download the updated context, including any new skills, modified instructions, or memory entries.

**Example — creating a skill:**

```
User: "帮我写一个调用 nano banana pro 画图的 skill"

CC Turn 1: WebSearch("nano banana pro API documentation")
  ← API docs

CC Turn 2: workspace_ls(path="skills/")
  ← existing skills list

CC Turn 3: workspace_write(
    path="skills/nano-banana-pro/skill.md",
    content="---\nname: nano-banana-pro\n...\n---\n\nGenerate images using..."
  )
  → cc-broker writes to local /tmp/cc-worker-xxx/claude-config/skills/nano-banana-pro/skill.md
  ← "Written successfully"

CC Turn 4: Reply "已创建 nano-banana-pro skill"

CC exits → diff detects new file → upload to OpenViking
  → viking://workspace/{wid}/claude-home/skills/nano-banana-pro/skill.md
  → next CC worker will discover this skill natively
```

### 15.7 Settings Configuration (unchanged from earlier)

The `settings.json` in OpenViking is pre-configured per workspace:

```json
{
  "permissions": {
    "allow": ["mcp__tool-router__*"]
  },
  "env": {
    "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "0"
  }
}
```

- `permissions.allow`: Whitelist MCP tools (supplementary to `--permission-mode bypassPermissions` CLI flag)
- `CLAUDE_CODE_AUTO_COMPACT_WINDOW=165000`: Set via env var at worker spawn (from nanoclaw's production config)
- `CLAUDE_CODE_DISABLE_AUTO_MEMORY=0`: Auto-memory **enabled** — CC natively manages MEMORY.md
- `bypassPermissions`: Set via CLI flag `--permission-mode bypassPermissions` (cannot be set in settings.json)

### 15.7 Worker Lifecycle

```go
func (b *CCBroker) spawnWorker(ctx context.Context, sessionID, workspaceID string) (*CCWorker, error) {
    // 1. Create local temp directory
    mountBase, _ := os.MkdirTemp("", "cc-worker-")
    claudeDir := filepath.Join(mountBase, "claude-config")
    projectDir := filepath.Join(mountBase, "project")
    os.MkdirAll(claudeDir, 0755)
    os.MkdirAll(projectDir, 0755)

    // 2. Download workspace context from OpenViking
    b.viking.Download(ctx, fmt.Sprintf("viking://workspace/%s/claude-home/", workspaceID), claudeDir)
    b.viking.Download(ctx, fmt.Sprintf("viking://workspace/%s/project/", workspaceID), projectDir)

    // 3. Deterministic auto-memory path (must be consistent across workers)
    autoMemPath := filepath.Join(claudeDir, "projects",
        fmt.Sprintf("ws_%s", workspaceID), "memory")
    os.MkdirAll(autoMemPath, 0755)

    // 4. Take file snapshot (for diff after CC exits)
    snapshot := takeFileSnapshot(claudeDir)

    // 5. Spawn CC worker
    bridgeURL := fmt.Sprintf("http://localhost:%d/v1/sessions/%s", b.bridgePort, sessionID)
    mcpConfigPath := b.writeMCPConfig(sessionID, workspaceID)

    cmd := exec.CommandContext(ctx, "claude",
        "--print",
        "--sdk-url", bridgeURL,
        "--tools", "WebSearch,WebFetch",
        "--mcp-config", mcpConfigPath,
        "--permission-mode", "bypassPermissions",
        "--dangerously-skip-permissions",
        "--no-session-persistence",
    )
    cmd.Dir = projectDir
    cmd.Env = []string{
        "CLAUDE_CONFIG_DIR=" + claudeDir,
        "CLAUDE_COWORK_MEMORY_PATH_OVERRIDE=" + autoMemPath,
        "ANTHROPIC_API_KEY=" + b.apiKey,
        "CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING=1",
        "CLAUDE_CODE_AUTO_COMPACT_WINDOW=165000",
        "HOME=" + mountBase,
        "PATH=" + os.Getenv("PATH"),
        "TERM=xterm-256color",
    }
    cmd.Start()

    // 6. After exit: diff + upload changes + cleanup
    go func() {
        cmd.Wait()

        // Find modified files and upload back to OpenViking
        changes := diffSnapshot(claudeDir, snapshot)
        for _, changed := range changes {
            content, _ := os.ReadFile(changed.Path)
            b.viking.Write(ctx,
                fmt.Sprintf("viking://workspace/%s/claude-home/%s", workspaceID, changed.RelPath),
                content)
        }

        os.RemoveAll(mountBase)
        os.Remove(mcpConfigPath)
    }()

    return &CCWorker{
        ID:        uuid.New().String(),
        Process:   cmd,
        SessionID: sessionID,
        Status:    WorkerStatusRunning,
        StartedAt: time.Now(),
    }, nil
}
```

Key design choices:
- **`CLAUDE_CONFIG_DIR`** instead of `HOME` override — more precise, only affects CC's config directory
- **`CLAUDE_COWORK_MEMORY_PATH_OVERRIDE`** — ensures all workers for the same workspace use the same auto-memory path, regardless of cwd
- **`--print`** — required for `--sdk-url` to work from CLI
- **`--permission-mode bypassPermissions --dangerously-skip-permissions`** — CLI flags (settings.json does not support `permissions.mode`)
- **No `--bare`** — not needed; downloaded files provide full `.claude/` structure; native features work
- **Snapshot + diff + upload** — only modified files are uploaded, minimizing OpenViking API calls

### 15.8 Subagent Context Consistency

Subagents are same-process async generators (not separate OS processes). They share the same local temp directory:

```
CC Worker 进程（single process, single filesystem）
  ├─ Main agent       ──┐
  ├─ Subagent A        ──┼── share the same /tmp/cc-worker-xxx/ directory
  └─ Subagent B        ──┘    local file writes are immediately visible to all
                              upload happens AFTER all agents complete
```

No context inconsistency between subagents within a single turn. Cross-turn consistency is guaranteed by the download-upload cycle: the next worker downloads the latest state (including changes from the previous turn).

### 15.9 Remaining Disabled Features

| Feature | Reason | Mechanism |
|---------|--------|-----------|
| Session transcript | Bridge event log is source of truth | `--no-session-persistence` |
| File attribution | No local files to track; tools execute on remote executors | `CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING=1` |
| Telemetry | No env var to disable in current CC version | Accepted risk |
| Cron (built-in) | Worker exits per turn; built-in cronScheduler non-functional | Replaced by MCP `create_scheduled_task` (Section 16.4) |
| Worktrees (built-in) | No local git repo | Remote worktree via `remote_bash` + `git worktree` (Section 16.3) |
| Keychain/OAuth | Managed environment | `ANTHROPIC_API_KEY` env var |

## 16. User Interaction in Headless Mode

### 16.1 AskUserQuestion — Async via IM Bridge

CC's `AskUserQuestion` tool requires user input. In headless mode, there's no terminal UI. We solve this by routing questions through the IM bridge asynchronously.

**Flow:**

```
CC calls AskUserQuestion("Which framework?", options=["React", "Vue"])
  │
  ▼
Tool Router MCP Server intercepts (this is NOT routed to an executor)
  │
  ▼
cc-broker forwards question to agentserver via SSE event:
  data: {"event_type":"user_question","payload":{"question":"...","options":[...]}}
  │
  ▼
agentserver sends question to user via imbridge:
  "CC 想问你一个问题：Which framework?\n1. React\n2. Vue\n请回复数字选择"
  │
  ▼
用户在微信回复: "1"
  │
  ▼
imbridge → agentserver detects this is a pending question response
  │
  ▼
agentserver injects answer into the bridge session as a user message:
  POST cc-broker /v1/sessions/{id}/worker/events/batch
  {"type":"user","message":{"content":"React"}}
  │
  ▼
CC receives the answer and continues reasoning
```

**Implementation in Tool Router:**

```go
// AskUserQuestion is handled by cc-broker, not by an executor
func (r *ToolRouter) HandleToolCall(ctx context.Context, req MCPToolCallRequest) (MCPToolResult, error) {
    if req.Name == "AskUserQuestion" {
        // Forward question to agentserver as a special event
        r.bridge.PublishQuestion(ctx, req.Meta["session_id"], req.Arguments)
        
        // Block until user responds (agentserver injects answer via bridge)
        answer, err := r.bridge.WaitForAnswer(ctx, req.Meta["session_id"], 
            timeout: 10*time.Minute)
        
        return MCPToolResult{
            Content: []ContentBlock{{Type: "text", Text: answer}},
        }, nil
    }
    // ... normal tool routing
}
```

**Pending question tracking in agentserver:**

```go
// agentserver tracks pending questions per session
type PendingQuestion struct {
    SessionID string
    Question  string
    Options   []string
    AskedAt   time.Time
}

func (s *Server) handleIMInbound(w http.ResponseWriter, r *http.Request) {
    // Check if there's a pending question for this session
    pending, exists := s.pendingQuestions[session.ID]
    if exists {
        // This message is an answer, not a new task
        s.ccBrokerClient.InjectAnswer(ctx, session.ID, msg.Content)
        delete(s.pendingQuestions, session.ID)
        w.WriteHeader(http.StatusAccepted)
        return
    }
    // ... normal message processing
}
```

### 16.2 Plan Mode — Async Approval via IM

Plan mode (`EnterPlanMode` → present plan → `ExitPlanMode` with user approval) follows the same async pattern as AskUserQuestion:

1. CC enters plan mode, writes plan content
2. cc-broker sends plan to user via IM: "这是我的实施计划：\n{plan}\n\n请回复'ok'批准或提出修改意见"
3. User replies "ok" → cc-broker injects approval → CC exits plan mode and starts implementation
4. User replies with feedback → cc-broker injects as user message → CC revises plan

### 16.3 Agent Tool — Subagents and Remote Worktree Isolation

CC's Agent tool spawns subagents as **same-process async generators** (not separate OS processes). Multiple subagents can run in parallel, each with independent conversation context but sharing the same MCP tools (Tool Router).

**What works natively:**
- Subagents share the parent's bridge session, FUSE mounts, and MCP config
- Multiple async subagents run concurrently via independent async generators
- Each subagent can call tools on any executor (executor_id is per-tool-call)
- Cross-executor parallelism: subagent A works on dev machine while subagent B works on GPU sandbox

**Worktree isolation — remote instead of local:**

CC's built-in `EnterWorktree` tool creates local git worktrees, which doesn't work in our design (no local repo). However, worktree isolation is **better** achieved on the remote executor via normal tool calls:

```
CC main agent: "帮我同时重构前端和后端"
  │
  ├─ Subagent A (background): "重构前端"
  │   remote_bash(executor="agt_dev", "git worktree add /tmp/wt-frontend -b refactor-frontend")
  │   remote_read(executor="agt_dev", file_path="/tmp/wt-frontend/src/App.tsx")
  │   remote_edit(executor="agt_dev", file_path="/tmp/wt-frontend/src/App.tsx", ...)
  │   remote_bash(executor="agt_dev", "cd /tmp/wt-frontend && npm test")
  │
  ├─ Subagent B (background): "重构后端"
  │   remote_bash(executor="agt_dev", "git worktree add /tmp/wt-backend -b refactor-backend")
  │   remote_read(executor="agt_dev", file_path="/tmp/wt-backend/cmd/server.go")
  │   remote_edit(executor="agt_dev", file_path="/tmp/wt-backend/cmd/server.go", ...)
  │   remote_bash(executor="agt_dev", "cd /tmp/wt-backend && go test ./...")
  │
  └─ Both complete → main agent merges:
     remote_bash(executor="agt_dev", "cd ~/repo && git merge refactor-frontend && git merge refactor-backend")
     remote_bash(executor="agt_dev", "git worktree remove /tmp/wt-frontend && git worktree remove /tmp/wt-backend")
```

This is **more powerful** than CC's built-in worktree isolation because:
- Worktrees can be created on **any executor**, not just the local machine
- Multiple subagents can work on the **same remote repo** with full git isolation
- CC acts as a central orchestrator, managing worktrees across executors via tool calls
- No special `EnterWorktree` tool needed — standard `Bash` + `git worktree` commands suffice

### 16.4 Scheduled Tasks — MCP Tool + agentserver Scheduler

CC's built-in `CronCreate` and `ScheduleWakeup` tools rely on an in-process scheduler that runs continuously. In our stateless design, CC workers exit per turn, making these built-in tools non-functional.

**Solution**: Replace the built-in cron tools with MCP tools that create tasks on agentserver's centralized scheduling service.

**Architecture:**

```
CC: "每天早上 9 点检查部署状态"
  │
  │ MCP tool call
  ▼
Tool Router MCP Server
  │
  │ scheduling tool → route to agentserver (not sandboxproxy)
  ▼
agentserver Scheduler Service
  │
  ├─ Store task in DB (workspace-scoped)
  └─ Scheduler daemon (persistent, in agentserver)
       │
       │ cron fires at 09:00
       ▼
     agentserver → cc-broker.ProcessTurn(prompt="检查部署状态")
       → CC worker starts → processes → exits
```

**MCP tool definitions:**

```json
[
  {
    "name": "create_scheduled_task",
    "description": "Create a scheduled task that runs a prompt on a cron schedule. Tasks persist across sessions and are managed by the server.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "cron": {
          "type": "string",
          "description": "5-field cron expression in local time (e.g., '0 9 * * *' = daily at 9am)"
        },
        "prompt": {
          "type": "string",
          "description": "The prompt to execute when the cron fires"
        },
        "recurring": {
          "type": "boolean",
          "default": true,
          "description": "true = fire on every cron match. false = fire once then auto-delete."
        },
        "description": {
          "type": "string",
          "description": "Human-readable description of the task"
        }
      },
      "required": ["cron", "prompt"]
    }
  },
  {
    "name": "list_scheduled_tasks",
    "description": "List all scheduled tasks for this workspace.",
    "inputSchema": {
      "type": "object",
      "properties": {}
    }
  },
  {
    "name": "cancel_scheduled_task",
    "description": "Cancel a scheduled task by ID.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "task_id": { "type": "string", "description": "The task ID to cancel" }
      },
      "required": ["task_id"]
    }
  }
]
```

**agentserver schema:**

```sql
scheduled_tasks (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    session_id    TEXT,              -- originating session (for context)
    cron          TEXT NOT NULL,     -- 5-field cron expression
    prompt        TEXT NOT NULL,
    description   TEXT,
    recurring     BOOLEAN DEFAULT TRUE,
    status        TEXT DEFAULT 'active',  -- 'active' | 'paused' | 'cancelled'
    last_fired_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ
)
```

**Advantages over CC's built-in CronCreate:**

| Aspect | CC built-in CronCreate | MCP Scheduling Service |
|--------|------------------------|------------------------|
| Storage | Local file `scheduled_tasks.json` | agentserver DB |
| Scheduler | In-process 1s timer (requires CC to be running) | agentserver daemon (always running) |
| Stateless compatibility | Broken (worker exits per turn) | Fully compatible |
| Visibility | Only within current CC session | Workspace-level; visible to all sessions and Web UI |
| Management | CC-internal only | agentserver API; extensible to Web UI, admin tools |

## 17. Feature Coverage Audit

Comprehensive audit of all CC features against the stateless design:

### 17.1 Fully Working Features

| Feature | Why It Works |
|---------|-------------|
| WebSearch / WebFetch | Preserved via `--tools "WebSearch,WebFetch"`; no executor needed |
| Skills | Downloaded to `.claude/skills/`; CC discovers natively; new skills created via `workspace_write` |
| Auto-Memory | FUSE-mounted MEMORY.md; CC reads/writes natively |
| CLAUDE.md | FUSE-mounted; full feature support (@include, frontmatter, rules) |
| Context Compaction | Auto + reactive compaction functional; memory persists via FUSE |
| Conversation Resume | Bridge SSE replays session history |
| Multi-Model | Agent tool model override + ConfigTool via FUSE settings |
| Streaming Output | Bridge SSE to frontend |
| Cost/Token Tracking | CC reports via bridge; cc-broker extracts from SDK messages |
| Config Tool | FUSE-mounted settings.json; changes persist via OpenViking |
| Image/PDF Reading | MCP Read tool must support binary content + base64 encoding |
| Concurrent Tool Execution | CC runs read-only tools in parallel; MCP server must be thread-safe |
| System Prompt Customization | Agent definitions via FUSE; `--append-system-prompt` for workspace-level |
| Permission System | `bypassPermissions` pre-configured in FUSE settings |

### 17.2 Working with Adaptation

| Feature | Adaptation |
|---------|-----------|
| AskUserQuestion | Async via IM bridge (Section 16.1) |
| Plan Mode | Async approval via IM (Section 16.2) |
| Agent Tool | Local subagents work natively; worktree isolation via remote `git worktree` on executor (more powerful than CC's built-in) |
| Edit (no undo) | Works but file checkpointing disabled; no undo |
| Error Recovery | Session-level via bridge; mid-turn crashes lose in-memory state |
| Scheduled Tasks (Cron) | Built-in CronCreate replaced by `create_scheduled_task` MCP tool → agentserver scheduler (Section 16.4) |

### 17.3 Intentionally Disabled

| Feature | Reason | Impact |
|---------|--------|--------|
| LSP | Requires local file indexing; incompatible with FUSE read-only cwd | Code navigation unavailable; CC can still read/grep files via MCP |
| Worktrees (built-in) | No local git repo; `EnterWorktree` tool not exposed | Remote worktree via `Bash` + `git worktree` on executor (Section 16.3) |
| CronCreate (built-in) | Worker exits per turn; built-in cronScheduler can't run | Replaced by `create_scheduled_task` MCP tool → agentserver scheduler (Section 16.4) |
| ScheduleWakeup (built-in) | Worker exits per turn | Replaced by `create_scheduled_task` with one-shot cron (Section 16.4) |
| File Attribution | No local files to track | Tool calls execute on remote executors |
| Telemetry | Prevent infrastructure leak | `CLAUDE_CODE_DISABLE_ANALYTICS=1` |
| Session Transcript | Bridge event log is source of truth | `--no-session-persistence` |
| RemoteTrigger | Requires OAuth to claude.ai | Not applicable for managed agents |
| Keychain/OAuth | Managed environment | `ANTHROPIC_API_KEY` env var |

## 18. Known Risks and Mitigations

### 18.1 MCP Tool Naming Collision (Resolved)

When `--tools "WebSearch,WebFetch"` disables most built-in tools, CC generates deny rules for the disabled tools. These deny rules also filter MCP tools matched by name. Additionally, allow rules in settings.json **cannot override** deny rules (deny is checked first in the permission evaluation order).

**Resolution**: All MCP tools use `remote_` prefix to avoid collision: `remote_bash`, `remote_read`, `remote_edit`, `remote_write`, `remote_glob`, `remote_grep`, `remote_ls`. This ensures they are not caught by CC's deny rules for built-in tools.

### 18.2 Token Security

Executor `tunnel_token` and `registry_token` should be hashed at rest (SHA-256) in the executor-registry database. Raw tokens are returned only once at registration time. sandboxproxy validates tunnel tokens by hashing the presented token and comparing against the stored hash.

### 18.3 Long-Running Tool Calls

Tool calls like model training may run for extended periods. The sandboxproxy `/api/execute` endpoint supports a configurable timeout per request. For commands expected to run longer than the timeout:

- The Tool Executor Agent should support an **async execution mode**: return a task ID immediately, provide a poll endpoint for status/output
- The Tool Router MCP Server exposes a corresponding `poll_task(executor_id, task_id)` tool for CC to check progress
- This is a Phase 2 enhancement; Phase 1 uses synchronous execution with a generous default timeout (10 minutes)

### 18.4 SSE Replay Limit

The existing bridge implementation has `sseReplayLimit = 1000` events. Sessions with more than 1000 events before compaction will lose early events on replay. Mitigations:
- `CLAUDE_CODE_AUTO_COMPACT_WINDOW=165000` triggers compaction before sessions grow too large
- cc-broker's bridge implementation should increase the replay limit or implement pagination
- Compaction reduces event count by summarizing old messages

### 18.5 Executor List Injection

The `list_executors` tool call costs a turn. As an optimization, the executor list can be injected into the CC system prompt via `--append-system-prompt` so CC always has current executor info without an explicit tool call. This can be implemented in cc-broker when constructing the CC worker startup command.

## 19. Related Work

- **InfiAgent** (arXiv, Jan 2026): File system as authoritative state record; agents reconstruct context from externalized state snapshots
- **"Externalization in LLM Agents"** (arXiv, Apr 2026): Unified review of memory/skills/protocols externalization; introduces "harness engineering" concept
- **LangGraph**: Checkpoint-based persistence at every node; PostgreSQL/Redis backends for horizontal scaling
- **Temporal.io**: Durable execution for tool calls as Activities; natural separation of reasoning (Workflow) from execution (Activity)
- **AgentFS** (Turso): SQLite-backed agent filesystem with S3 sync; checkpointable agent state
- **OpenAI Agents SDK**: Runner pattern with `ModelSettings(store=False)` for stateless execution
- **MCP (Model Context Protocol)**: Standard tool abstraction layer; remote MCP servers for production deployments
