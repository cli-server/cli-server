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

### 1.3 Key Insight

Claude Code supports `--tools ""` to disable all built-in tools, combined with `--mcp-config` to load a custom MCP server. This enables a **zero-intrusion** approach: CC's agent loop (reasoning, compaction, system prompt assembly) is fully preserved, while all tool side effects are redirected through our custom Tool Router MCP Server.

```bash
claude --tools "" \
       --mcp-config '{"mcpServers":{"tool-router":{"type":"http","url":"..."}}}' \
       --print --output-format stream-json \
       --bare --no-session-persistence \
       "user prompt"
```

## 2. Architecture

### 2.1 Service Topology

```
用户(微信/Web/API)
  │
  ▼
imbridge ──► agentserver ──► cc-broker ──► sandboxproxy
             (用户业务)  HTTP (推理编排) HTTP (连接/执行)
                │                           │    │
                │                        tunnel  HTTP
                │                           │    │
                │                           ▼    ▼
                │                       Local   Sandbox
                │                       Agent   (K8s/Docker)
                │                         │
                │                         │ 注册/心跳
                │                         ▼
                │                   executor-registry
                │                   (executor 生命周期)
                │
            PostgreSQL
         (共享 event log)
```

### 2.2 Service Responsibilities

| Service | Responsibilities | Does NOT handle |
|---------|-----------------|-----------------|
| **agentserver** | User auth, workspace/session CRUD, IM inbound routing, event log persistence, bridge SSE to frontend | CC execution, executor connectivity |
| **cc-broker** | CC Worker Pool, context materialization (DB→JSONL), Tool Router MCP Server, calling sandboxproxy for tool execution | Tunnel management, business logic, executor lifecycle |
| **sandboxproxy** | Tunnel management (WebSocket), sandbox HTTP connectivity, unified tool execution API | Business logic, CC reasoning, executor registration |
| **executor-registry** | Executor registration (OAuth), heartbeat, capability storage, capability probe triggering | Tunnel management, tool execution |
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

    // 2. Load session event history from DB
    history, _ := s.db.GetSessionEvents(r.Context(), session.ID)

    // 3. Query available executors in workspace
    executors, _ := s.executorRegistryClient.ListExecutors(r.Context(), workspaceID)

    // 4. Async: call cc-broker (don't block HTTP response)
    go s.processWithCCBroker(r.Context(), session, history, executors, msg)

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
    history []SessionEvent, executors []ExecutorInfo, msg IMInboundMessage) {

    // 1. Call cc-broker ProcessTurn (SSE stream)
    eventStream, err := s.ccBrokerClient.ProcessTurn(ctx, ProcessTurnRequest{
        SessionID:   session.ID,
        WorkspaceID: session.WorkspaceID,
        UserMessage: msg.Content,
        History:     history,
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

## 4. Context Management (Event Sourcing)

### 4.1 Storage

All session messages are stored as append-only events in PostgreSQL, reusing the existing `agent_session_events` table:

```sql
agent_session_events (
    id            BIGSERIAL,          -- global sequence number
    session_id    TEXT,                -- owning session
    event_id      TEXT UNIQUE,         -- dedup ID
    event_type    TEXT,                -- 'message' | 'metadata' | 'compaction_boundary'
    source        TEXT,                -- 'user' | 'assistant' | 'system' | 'tool_result'
    epoch         INTEGER,            -- which worker wrote this
    payload       JSONB,              -- CC SerializedMessage (stored as-is)
    ephemeral     BOOLEAN,            -- transient messages (not persisted)
    created_at    TIMESTAMPTZ
)
```

### 4.2 Materialization (DB → JSONL)

Before each CC worker invocation, cc-broker materializes the session event log into a JSONL file that CC can load via `--resume`:

```go
func materializeSession(ctx context.Context, db *DB, sessionID string) (string, error) {
    // Optimization: if compaction boundary exists, only load events after it
    boundary, _ := db.GetLatestCompactionBoundary(ctx, sessionID)

    var events []SessionEvent
    if boundary != nil {
        events, _ = db.GetSessionEventsAfter(ctx, sessionID, boundary.Sequence)
    } else {
        events, _ = db.GetSessionEvents(ctx, sessionID,
            WithNonEphemeral(),
            WithOrderBySequence())
    }

    // Write to CC's expected session path
    path := filepath.Join(ccLogsDir, projectHash,
        fmt.Sprintf("session-%s.jsonl", sessionID))

    f, _ := os.Create(path)
    defer f.Close()

    if boundary != nil {
        f.Write(boundary.Payload) // summary at top
        f.Write([]byte("\n"))
    }
    for _, evt := range events {
        f.Write(evt.Payload) // payload is CC's SerializedMessage format
        f.Write([]byte("\n"))
    }
    return path, nil
}
```

### 4.3 Write Path

After CC processes a turn, cc-broker captures the streaming NDJSON output and forwards each event to agentserver, which persists them:

```
CC stdout (stream-json NDJSON)
  → cc-broker parses each line
  → SSE stream to agentserver
  → agentserver writes each event to agent_session_events
  → agentserver broadcasts via bridge SSE to frontend
```

### 4.4 Compaction

CC's built-in auto-compaction remains functional. When conversation grows too long:

1. CC triggers compaction (LLM summarizes old messages)
2. CC outputs a `compaction_boundary` event
3. agentserver persists the boundary to DB
4. Next materialization loads only boundary summary + subsequent events

## 5. Tool Router MCP Server

### 5.1 Architecture

The Tool Router MCP Server runs inside cc-broker. It exposes the same tool interfaces as CC's built-in tools, but routes actual execution to remote executors via sandboxproxy.

```
CC Worker
  │  MCP protocol (HTTP)
  ▼
Tool Router MCP Server (in cc-broker)
  │
  ├─ list_executors         → query executor-registry
  ├─ Bash(executor_id, ...) → POST sandboxproxy /api/execute
  ├─ Edit(executor_id, ...) → POST sandboxproxy /api/execute
  ├─ Read(executor_id, ...) → POST sandboxproxy /api/execute
  └─ ...
```

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

All standard CC tools are exposed with an additional `executor_id` parameter. Example (Bash):

```json
{
  "name": "Bash",
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
| `Bash` | `executor_id`, `command`, `timeout` | Execute shell command |
| `Read` | `executor_id`, `file_path`, `offset`, `limit` | Read file |
| `Edit` | `executor_id`, `file_path`, `old_string`, `new_string` | Edit file |
| `Write` | `executor_id`, `file_path`, `content` | Write file |
| `Glob` | `executor_id`, `pattern`, `path` | File pattern search |
| `Grep` | `executor_id`, `pattern`, `path`, `glob` | Content search |
| `LS` | `executor_id`, `path` | List directory |

### 5.3 Permission Enforcement

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

CC Turn 2: Read(executor_id="agt_dev", file_path="~/app/deploy.sh")
  → Tool Router → sandboxproxy → tunnel → dev machine
  ← file content

CC Turn 3: Bash(executor_id="sbx_test", command="bash deploy.sh")
  → Tool Router → sandboxproxy → HTTP → test sandbox pod
  ← deployment output

CC Turn 4: Reply "部署完成，以下是输出..."
```

## 6. CC Broker

### 6.1 API

```
POST /api/turns
Content-Type: application/json

{
    "session_id": "cse_xxx",
    "workspace_id": "ws_xxx",
    "user_message": "帮我修复这个 bug",
    "history": [ ... session events ... ],
    "executors": [ ... executor list ... ]
}

Response: Content-Type: text/event-stream

data: {"event_type":"assistant_message","payload":{...}}
data: {"event_type":"tool_use","payload":{...}}
data: {"event_type":"tool_result","payload":{...}}
data: {"event_type":"assistant_message","payload":{...}}
data: {"event_type":"done","payload":{...}}
```

### 6.2 Worker Pool

CC broker maintains a pool of headless CC processes for reuse:

```go
type WorkerPool struct {
    mu        sync.Mutex
    idle      chan *CCWorker
    poolSize  int
    maxUsage  int              // recycle worker after N requests
    mcpConfig string           // Tool Router MCP Server config
}

type CCWorker struct {
    ID         string
    Process    *exec.Cmd
    Stdin      io.WriteCloser
    Stdout     io.ReadCloser
    Status     WorkerStatus   // idle | busy | draining | dead
    UsageCount int
}
```

### 6.3 Worker Startup

```go
func (p *WorkerPool) spawnWorker() (*CCWorker, error) {
    cmd := exec.Command("claude",
        "--print",
        "--input-format", "stream-json",
        "--output-format", "stream-json",
        "--tools", "",                    // disable all built-in tools
        "--bare",                         // minimal init (no hooks, no LSP, no plugins)
        "--no-session-persistence",       // no local disk writes
        "--mcp-config", p.mcpConfigPath,  // Tool Router MCP Server
    )
    cmd.Env = append(os.Environ(),
        "ANTHROPIC_API_KEY="+p.apiKey,
        "CLAUDE_CODE_SIMPLE=1",
    )

    stdin, _ := cmd.StdinPipe()
    stdout, _ := cmd.StdoutPipe()
    cmd.Start()

    return &CCWorker{
        ID:      uuid.New().String(),
        Process: cmd,
        Stdin:   stdin,
        Stdout:  stdout,
        Status:  WorkerStatusIdle,
    }, nil
}
```

### 6.4 Request Processing

```go
func (p *WorkerPool) HandleTurn(ctx context.Context, req ProcessTurnRequest) (<-chan TurnEvent, error) {
    // 1. Acquire idle worker
    worker := <-p.idle
    worker.Status = WorkerStatusBusy

    // 2. Materialize context (DB events → JSONL file)
    ctxPath, _ := materializeSession(ctx, p.db, req.SessionID, req.History)

    // 3. Generate dynamic MCP config (inject session_id for permission scoping)
    mcpConfig := p.buildMCPConfig(req.SessionID, req.WorkspaceID)

    // 4. Send input to CC worker stdin
    input := StreamInput{
        Resume:      ctxPath,
        Prompt:      req.UserMessage,
        MCPConfig:   mcpConfig,
    }
    json.NewEncoder(worker.Stdin).Encode(input)

    // 5. Stream output as TurnEvents
    events := make(chan TurnEvent)
    go func() {
        defer close(events)
        defer p.returnWorker(worker)
        defer os.Remove(ctxPath)

        scanner := bufio.NewScanner(worker.Stdout)
        for scanner.Scan() {
            var event TurnEvent
            json.Unmarshal(scanner.Bytes(), &event)
            events <- event
        }
    }()

    return events, nil
}

func (p *WorkerPool) returnWorker(w *CCWorker) {
    w.UsageCount++
    if w.UsageCount >= p.maxUsage {
        w.Process.Kill()
        go func() {
            newW, _ := p.spawnWorker()
            p.idle <- newW
        }()
        return
    }
    w.Status = WorkerStatusIdle
    p.idle <- w
}
```

### 6.5 Health Management

```go
func (p *WorkerPool) healthLoop() {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        p.mu.Lock()
        for i, w := range p.workers {
            // Replace exited processes
            if w.Process.ProcessState != nil && w.Process.ProcessState.Exited() {
                p.workers[i], _ = p.spawnWorker()
            }
            // Drain workers with high memory usage (Node.js)
            if getProcessMemoryMB(w.Process.Process.Pid) > p.maxMemoryMB {
                w.Status = WorkerStatusDraining
            }
        }
        // Replenish pool to target size
        for len(p.workers) < p.poolSize {
            w, _ := p.spawnWorker()
            p.workers = append(p.workers, w)
            p.idle <- w
        }
        p.mu.Unlock()
    }
}
```

### 6.6 Horizontal Scaling

Each cc-broker instance runs its own worker pool. Requests can be routed to any cc-broker instance since all state is in PostgreSQL:

```
cc-broker-1 (pool: 4 CC workers)  ←─┐
cc-broker-2 (pool: 4 CC workers)  ←─┤── Load Balancer
cc-broker-3 (pool: 4 CC workers)  ←─┘
                                       ↕
                                  PostgreSQL
```

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
   - Materializes context (empty) → JSONL
   - Acquires CC worker from pool
   - Starts CC reasoning

⑤ CC Agent Loop:
   Turn 1: list_executors
     → Tool Router → executor-registry
     ← [Dev Machine Agent, GPU Sandbox]

   Turn 2: Read(executor_id="agt_dev", file_path="~/projects/ml/train.py")
     → Tool Router → sandboxproxy → tunnel → Dev Machine
     ← file content (train.py)

   Turn 3: Read(executor_id="agt_dev", file_path="~/projects/ml/requirements.txt")
     → Tool Router → sandboxproxy → tunnel → Dev Machine
     ← file content (requirements.txt)

   Turn 4: Bash(executor_id="sbx_gpu", command="pip install -r /tmp/requirements.txt")
     → Tool Router → sandboxproxy → HTTP → GPU Sandbox
     ← pip install output

   Turn 5: Bash(executor_id="sbx_gpu", command="python /tmp/train.py --epochs 10")
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
| Statelessness | Externalize context + tools | CC process is disposable, any worker handles any session |
| Context storage | Event Sourcing (PostgreSQL) | Reuse existing `agent_session_events`, append-only, replayable |
| Tool routing | MCP Server with `--tools ""` | CC-native, zero intrusion, clean separation |
| Executor selection | CC (LLM) decides | LLM understands executor capabilities, makes intelligent routing |
| Capability discovery | CC-driven probing | Flexible, no agent binary updates needed |
| Service split | 4 services | Clean separation of concerns, independent scaling |
| Inter-service protocol | HTTP + SSE | Consistent with existing codebase, no new dependencies |
| Worker management | Process pool with recycling | Avoid cold-start overhead, prevent memory leaks |
| Permission model | Workspace isolation | Session can only access executors in its own workspace |

## 12. New Components Summary

| Component | Location | Scope |
|-----------|----------|-------|
| cc-broker service | New Go service | Core: worker pool, context materialization, Tool Router MCP |
| sandboxproxy service | New Go service (or extract from agentserver) | Tunnel management, unified tool execution API |
| executor-registry service | New Go service | Executor registration, heartbeat, capability storage |
| Tool Executor Agent | In sandbox images + agentserver-agent binary | Lightweight HTTP handler for tool execution |
| IM inbound endpoint | agentserver addition | `POST /api/workspaces/{wid}/im/inbound` |

## 13. Migration Path

1. **Phase 1**: Build executor-registry + sandboxproxy as standalone services
2. **Phase 2**: Build cc-broker with worker pool and Tool Router MCP Server
3. **Phase 3**: Add IM inbound endpoint to agentserver, rewire imbridge
4. **Phase 4**: Modify agentserver-agent to register with executor-registry and connect tunnel to sandboxproxy
5. **Phase 5**: Deprecate per-agent CC instances, route all reasoning through cc-broker

## 14. Related Work

- **InfiAgent** (arXiv, Jan 2026): File system as authoritative state record; agents reconstruct context from externalized state snapshots
- **"Externalization in LLM Agents"** (arXiv, Apr 2026): Unified review of memory/skills/protocols externalization; introduces "harness engineering" concept
- **LangGraph**: Checkpoint-based persistence at every node; PostgreSQL/Redis backends for horizontal scaling
- **Temporal.io**: Durable execution for tool calls as Activities; natural separation of reasoning (Workflow) from execution (Activity)
- **AgentFS** (Turso): SQLite-backed agent filesystem with S3 sync; checkpointable agent state
- **OpenAI Agents SDK**: Runner pattern with `ModelSettings(store=False)` for stateless execution
- **MCP (Model Context Protocol)**: Standard tool abstraction layer; remote MCP servers for production deployments
