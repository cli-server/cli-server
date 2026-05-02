# cc-broker Worker via Claude Agent SDK

**Date:** 2026-05-02
**Status:** Draft
**Author:** mryao + Claude

**Supersedes:** §2 (CC invocation via `--sdk-url`) and §4 (Bridge SSE replay as context loading) of [`2026-04-15-stateless-cc-design.md`](2026-04-15-stateless-cc-design.md).
The routing_mode, executor-registry, OpenViking download-run-upload, and IM / scheduling / workspace tool sections of that spec remain in effect.

## 1. Overview

### 1.1 Problem Statement

The original stateless CC spec §2 assumed each turn would run `claude --sdk-url http://cc-broker:8080/v1/sessions/{id}` so the CC worker could pull conversation history from cc-broker's bridge SSE endpoint and write results back via HTTP. In practice, Claude CLI 2.1.126 rejects this at startup:

```
Error: --sdk-url rejected: host "127.0.0.1" is not an approved Anthropic endpoint.
This flag is reserved for Remote Control worker processes connecting to Anthropic's backend.
```

The `--sdk-url` host allowlist is a hard-coded Set inside the CLI binary — 5 Anthropic/FedStart hosts, `wss://` or `https://` only. No env var, config, or flag overrides it. `--sdk-url` is an internal Anthropic Remote Control protocol, not a public extension point.

The bridge architecture is therefore unreachable with stock `claude` CLI. All of cc-broker's bridge HTTP endpoints (`/v1/sessions/{id}/bridge`, `/worker/events/{stream,batch}`, `/worker/heartbeat`, etc.) have zero real callers — their intended consumer (the CLI) refuses to talk to them.

### 1.2 Goal

Replace cc-broker's worker path with the [Claude Agent SDK for Go](https://github.com/agentserver/claude-agent-sdk-go), which wraps Claude CLI via stdio and needs no external bridge. Preserve every externally observable behavior: `POST /api/turns` still accepts the same payload and streams back the same SSE events; `agent_session_events` still captures the conversation for frontend replay; the IM routing loop from imbridge → agentserver → cc-broker is untouched.

Internally, the change is a clean rewrite of the cc-broker worker layer:

- Bridge HTTP endpoints deleted (dead code).
- `SpawnWorker` replaced by in-process agent loop using `agentsdk.NewClient`.
- Tool Router HTTP MCP server replaced by in-process SDK MCP server (Go function callbacks).
- OpenViking download-run-upload kept, now also covering CLI session `.jsonl` files so conversation history resumes through SDK `WithResume(sessionID)`.

### 1.3 Non-Goals

- Dropping Claude CLI entirely (e.g., rewriting as a direct `anthropic-sdk-go` loop). The SDK is a thin CLI wrapper and is the officially supported agent path.
- Changing `agent_sessions` / `agent_session_events` schema. Those tables continue to exist for SSE broadcast and audit — just no longer as the history-replay source.
- Touching routing_mode, executor-registry, or IM layers — those are orthogonal and already work.
- Session-affinity pooling. Each turn still spawns and exits a fresh CLI process; cc-broker stays horizontally scalable.
- Fixing OpenViking tenant auth headers (`X-OpenViking-Account` / `X-OpenViking-User`). Pre-existing, tracked separately — without this fix, diff-upload keeps failing and context persistence remains broken, but conversations within a single session still work (CLI session file stays in OpenViking for the duration of the turn).

### 1.4 Relation to prior spec

This spec is a targeted replacement for two sections of `2026-04-15-stateless-cc-design.md`:

- **§2 (Architecture / CC invocation)**: The `--sdk-url` command line is invalid. The new command line is `agentsdk.NewClient(opts...).Connect()`, which internally runs `claude --print` with stdio framing. No external URL, no bridge.
- **§4 (Context Management via Bridge SSE)**: Conversation history is not replayed to CC via SSE. It lives in the Claude CLI session `.jsonl` file inside OpenViking and is loaded by `WithResume(sessionID)`. `agent_session_events` remains as an independent log for frontend SSE + audit.

All other sections of the prior spec remain unchanged.

### 1.5 SDK version alignment

This spec tracks **both** the [Claude Agent SDK TypeScript V1](https://code.claude.com/docs/en/agent-sdk/typescript) (stable) and [V2 preview](https://code.claude.com/docs/en/agent-sdk/typescript-v2-preview) (`unstable_v2_*`) interfaces. Implementation starts on V1 because that is what `claude-agent-sdk-go` binds today; `runner/` is designed as a thin adapter so switching cc-broker to V2 only touches one file once the Go SDK adds V2 bindings.

#### Option / concept mapping

| Semantic | TS V1 | TS V2 preview | Go SDK (V1) | Go SDK (V2, future) |
|---|---|---|---|---|
| One-shot prompt | `query({prompt, options})` (async generator) | `unstable_v2_prompt(prompt, options)` → `Promise<SDKResultMessage>` | `agentsdk.Query(ctx, prompt, opts...)` → `*Stream` | e.g. `agentsdk.V2Prompt(ctx, prompt, opts...)` |
| Create session | — (sessions implicit in `query`) | `unstable_v2_createSession(options)` → `SDKSession` | `agentsdk.NewClient(opts...)` + `Connect(ctx)` | e.g. `agentsdk.V2CreateSession(ctx, opts...)` |
| Resume session by ID | `query({options: {resume: sessionId}})` | `unstable_v2_resumeSession(sessionId, options)` | `agentsdk.NewClient(agentsdk.WithResume(id), ...)` | e.g. `agentsdk.V2ResumeSession(ctx, id, opts...)` |
| Send user message | Input async iterable yields `SDKUserMessage` | `session.send(message)` | `client.Query(msg)` / `client.SendMessage(msg)` | `session.Send(ctx, msg)` |
| Stream output | Loop over `query()` generator | `for await (const msg of session.stream())` | `for stream.Next(); stream.Current()` | `for msg := range session.Stream()` |
| Close | `q.close()` | `session.close()` / `await using` | `client.Close()` / `stream.Close()` | `session.Close()` |
| Permission bypass | `permissionMode: "bypassPermissions"` + `allowDangerouslySkipPermissions: true` | same (V2 carries options forward) | `WithPermissionMode(PermissionBypassAll)` + `WithAllowDangerouslySkipPermissions()` | same pair |
| System prompt | `systemPrompt: string \| preset obj` | same | `WithSystemPrompt` / `WithSystemPromptPreset` / `WithSystemPromptFile` | same |
| MCP servers | `mcpServers: Record<string, McpServerConfig>` | same | `WithMcpServers(map[string]McpServerConfig)` | same |
| In-process MCP tool | `tool(name, desc, zodSchema, handler)` | same | `agentsdk.Tool[T](name, desc, handler)` (reflection → JSON schema) | same |
| Fork session | `forkSession: true` | **not supported** in V2 preview | `WithForkSession()` | n/a until V2 adds it |

#### Phasing

- **Phase 1 (this spec's plan):** implement `runner/` against V1 (`agentsdk.NewClient` + `Connect` + `Client.Query` + `WithResume`). This is the only runtime path stock Go SDK can drive today.
- **Phase 2 (when Go SDK ships V2 bindings):** add a V2 adapter inside `runner/runner.go`. The external contract of `runner.Run(ctx, ws, session, userMessage, mcpServer) (<-chan agentsdk.SDKMessage, error)` is V2-friendly: V2's `session.Send + session.Stream()` maps to the same function signature with no change to callers. The V1→V2 migration is contained to `runner/` and `runner/options.go`.
- **V2 preview caveats we inherit:** `forkSession` does not exist in V2. We do not use `WithForkSession()` in Phase 1, so we won't regress when migrating. If a future feature needs fork, keep the V1 path alive for that code path until V2 gains support.

#### Runner adapter seam

To keep the V1→V2 migration a one-file change:

```go
// runner/runner.go — unexported interface, swap implementations by build tag or config
type sdkSession interface {
    Send(ctx context.Context, msg string) error
    Messages() <-chan agentsdk.SDKMessage
    Close() error
}

// V1 implementation today:
type v1Session struct {
    client *agentsdk.Client
    msgCh  <-chan agentsdk.SDKMessage
}

// V2 implementation (stub, swap in when available):
// type v2Session struct {
//     session agentsdk.V2Session
// }
```

No caller outside `runner/` ever sees V1 or V2 directly. `handler_turns.go` consumes `runner.Run(...)`'s returned `SDKMessage` channel; `tools/` knows nothing about V1/V2.

#### Which to ship first

V1. V2 is explicitly labelled "unstable preview" and `claude-agent-sdk-go` has no V2 bindings. Shipping V1 now unblocks production; V2 is a follow-up once the Go SDK catches up and V2 drops the preview label.

## 2. Architecture

cc-broker receives `POST /api/turns` (unchanged), orchestrates a single turn, streams SSE back. Turn orchestration is the same shape as before; only the middle ("spawn worker + bridge loop") changes:

```
POST /api/turns                                          ← agentserver (SSE response)
  │
  ▼
handler_turns.go
  ├─ TurnLock.Acquire(session_id)
  ├─ Insert user message → agent_session_events
  │
  ▼
workspace.Setup(ctx, workspaceID, sessionID) → *Workspace
  ├─ mkdir /tmp/cc-worker-<uuid>/{claude-config,project}
  ├─ viking.DownloadTree(".../claude-home/", ClaudeDir)
  │     ↳ Includes CLI session file at
  │       claude-home/.claude/projects/<proj_hash>/<sessionID>.jsonl
  ├─ viking.DownloadTree(".../project/", ProjectDir)
  ├─ Snapshot := takeFileSnapshot(ClaudeDir)
  │
  ▼
tctx := tools.NewContext(sessionID, workspaceID, imChannelID, imUserID, ...)
mcpServer := tools.BuildMcpServer(tctx)
  │
  ▼
runner.Run(ctx, ws, session, userMessage, mcpServer) → <-chan SDKMessage
  ├─ opts := []QueryOption{
  │     WithCwd(ws.ProjectDir),
  │     WithEnv("CLAUDE_CONFIG_DIR", ws.ClaudeDir,
  │             "CLAUDE_COWORK_MEMORY_PATH_OVERRIDE", ws.MemoryDir,
  │             "ANTHROPIC_API_KEY" or "ANTHROPIC_AUTH_TOKEN" + "ANTHROPIC_BASE_URL"
  │             forwarded from cc-broker process env,
  │             "CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING=1",
  │             "CLAUDE_CODE_AUTO_COMPACT_WINDOW=165000"),
  │     WithResume(sessionID),          ← SDK loads session .jsonl into CLI
  │     WithSystemPrompt(buildSystemPrompt(workspaceID)),
  │     WithMcpServers({"cc-broker": {SDK: mcpServer}}),
  │     WithAllowedTools("WebSearch", "WebFetch", "mcp__cc-broker__*"),
  │     WithPermissionMode(PermissionBypassAll),
  │     WithAllowDangerouslySkipPermissions(),  ← required alongside BypassAll
  │     WithMaxTurns(config.maxTurns),
  │ }
  ├─ client := agentsdk.NewClient(opts...)
  ├─ client.Connect(ctx)               ← spawn `claude --print ...` stdio subprocess
  ├─ client.Query(userMessage)
  └─ fan SDK messages out to channel
  │
  ▼
handler_turns.go consumes channel:
  ├─ for evt := range ch:
  │     payload := runner.events.ToEventPayload(evt)
  │     store.InsertEvents(payload)
  │     sse.Broadcast(sessionID, evt)   ← streams through agentserver to frontend/IM
  ├─ on result / ctx done / disconnect:
  │     client.Close(); kick Teardown
  │
  ▼
workspace.Teardown(ctx, ws)            [runs in background goroutine]
  ├─ changed := ws.DiffSnapshot()      ← MEMORY.md, skills, CLAUDE.md, session .jsonl
  ├─ viking.Upload(each changed file)
  └─ os.RemoveAll(ws.TempDir)
```

Key replacements vs prior architecture:

| Prior | New |
|---|---|
| `claude --sdk-url http://cc-broker:8080/v1/sessions/{id}` + `--mcp-config /tmp/mcp.json` | `agentsdk.NewClient(WithResume(id), WithMcpServers({"cc-broker": {SDK: mcpServer}}))` |
| HTTP MCP server on localhost port; CC issues HTTP calls | In-process Go functions; SDK control protocol routes calls over stdio |
| CC writes events back via `POST /worker/events/batch` (bridge API) | cc-broker reads SDK messages from stdio directly via the SDK's channel |
| Session history reconstructed by CC from bridge SSE replay | CLI session `.jsonl` file downloaded from OpenViking; `WithResume(sessionID)` |
| `agent_session_events` is source of truth for CC history | `agent_session_events` is source of truth for **frontend SSE + audit**; CLI session file is source of truth for **LLM context** |

## 3. File Structure

### 3.1 New layout of `internal/ccbroker/`

```
internal/ccbroker/
├── server.go                   # HTTP router (slimmed — no bridge routes)
├── config.go                   # unchanged
├── store.go                    # unchanged (agent_sessions + agent_session_events)
├── sse.go                      # unchanged
├── turn_lock.go                # unchanged
├── dedup.go                    # unchanged
├── models.go                   # unchanged
├── handler_turns.go            # rewritten to call workspace.Setup / runner.Run / Teardown
├── handler_session.go          # unchanged
├── migrations/                 # unchanged
│
├── workspace/                  # NEW
│   ├── workspace.go            #   type Workspace { TempDir, ClaudeDir, ProjectDir, MemoryDir, snapshot }
│   ├── mount.go                #   Setup(ctx, wid, sid, viking) (*Workspace, error)
│   ├── viking_client.go        #   moved from ccbroker/viking_client.go
│   └── snapshot.go             #   takeFileSnapshot, diffSnapshot
│
├── runner/                     # NEW
│   ├── runner.go               #   Run(ctx, ws, sess, userMsg, mcp) (<-chan agentsdk.SDKMessage, error)
│   ├── options.go              #   buildClientOptions(ws, sess, mcp, cfg) []agentsdk.QueryOption
│   └── events.go               #   ToEventPayload(agentsdk.SDKMessage) (json.RawMessage, error)
│
└── tools/                      # NEW
    ├── context.go              #   type Context struct { SessionID, WorkspaceID, IMChannelID,
    │                           #       IMUserID, ExecutorRegistryURL, AgentserverURL,
    │                           #       Viking *workspace.VikingClient, ClaudeDir string }
    ├── router.go               #   BuildMcpServer(*Context) *agentsdk.McpSdkServer
    ├── executor.go             #   remote_bash, remote_read, remote_edit, remote_write,
    │                           #       remote_glob, remote_grep, remote_ls, list_executors
    ├── workspace.go            #   workspace_read, workspace_write, workspace_ls
    ├── im.go                   #   send_message, send_image, send_file
    ├── scheduler.go            #   create_scheduled_task, list_scheduled_tasks,
    │                           #       cancel_scheduled_task  (stubbed; agentserver side not built yet)
    └── askuser.go              #   AskUserQuestion (routed to agentserver pending-questions queue)
```

### 3.2 Files being deleted

These served the `--sdk-url` bridge model and have no role in the SDK path:

- `handler_bridge.go` — `POST /v1/sessions/{id}/bridge` worker attach
- `handler_events.go` — `GET /worker/events/stream` + `POST /worker/events`
- `handler_internal_events.go` — `POST /worker/internal-events`
- `handler_worker.go` — `PUT /worker/` + `POST /worker/heartbeat`
- `jwt.go` — bridge-only auth helpers
- `middleware.go` — bridge-only middleware
- `mcp_server.go` — HTTP MCP server
- `mcp_router.go` — HTTP-path routing (replaced by `tools/router.go`)
- `mcp_tools.go` — HTTP tool definitions (replaced by `tools/`)
- `mcp_router_im_test.go`, `mcp_server_test.go` — corresponding tests
- `worker.go` — responsibilities split into `workspace/` + `runner/`
- `viking_client.go` — moved into `workspace/`
- `worker_test.go` — supplanted by `workspace/` and `runner/` tests
- `integration_test.go` — any bridge-flavoured assertions rewritten against the new turn flow

### 3.3 Subpackage responsibilities

**`workspace/`** — ephemeral local view of a workspace for one turn. No Claude SDK imports.

- `Workspace` struct: paths + initial snapshot
- `Setup(ctx, wid, sid, viking)`: create temp dirs, download claude-home + project trees, ensure `memory/` subtree, snapshot claude-home
- `Teardown(ctx, ws, viking)`: diff snapshot, upload changes, remove temp dir

**`runner/`** — Claude Agent SDK lifecycle per turn. No DB or HTTP imports beyond SDK.

- `Run(ctx, ws, session, userMessage, mcpServer) (<-chan agentsdk.SDKMessage, error)`
- `options.go` assembles `QueryOption` slice from ws + cc-broker config + env
- `events.go` converts each `SDKMessage` into a `json.RawMessage` payload for storage / SSE

**`tools/`** — in-process MCP tools. Depends on executor-registry client, agentserver client, viking client, `*Workspace` filesystem.

- `Context` carries per-turn identity + dependencies; handlers close over it
- Each tool is `agentsdk.Tool[T](name, description, handler)` with typed input, auto-generated JSON schema
- `BuildMcpServer(*Context)` assembles all tools into a single `*McpSdkServer`

**`handler_turns.go`** — external-facing orchestration. Unchanged contract, simpler body:

1. Acquire lock → insert user event
2. `workspace.Setup` → defer `workspace.Teardown`
3. `tools.BuildMcpServer(tctx)` → `runner.Run`
4. Pump `SDKMessage` channel into store + SSE broadcaster
5. Emit `done` sentinel, release lock

## 4. Data Flow

A full turn, from imbridge receiving a WeChat message through to the reply going back:

1. **imbridge** forwards to `agentserver POST /api/workspaces/{wid}/im/inbound` (routing_mode=stateless_cc).
2. **agentserver** resolves or creates session via chat_jid → `POST cc-broker /api/turns` with `{session_id, workspace_id, user_message, im_channel_id, im_user_id}` (unchanged).
3. **cc-broker handler_turns.go**:
   a. Acquires `TurnLock` per session.
   b. Writes the user message as an `agent_session_events` row with `SDKUserMessage`-shaped payload.
   c. Calls `workspace.Setup`. The CLI session file `.../projects/<proj_hash>/<sessionID>.jsonl` lands in `ClaudeDir`.
   d. Builds `tools.Context` with all per-turn dependencies.
   e. Constructs `mcpServer := tools.BuildMcpServer(tctx)` — a single `*McpSdkServer` with ~16 tools.
   f. Calls `runner.Run(ctx, ws, session, userMessage, mcpServer)` which:
      - Builds `QueryOption`s including `WithResume(sessionID)`, `WithCwd(ws.ProjectDir)`, env vars (config dir, memory override, Anthropic credentials), MCP server, allowed tool list, permission mode.
      - Creates `agentsdk.Client`, `Connect`s (starts `claude --print` stdio), then calls `client.Query(userMessage)`.
      - Returns a `<-chan agentsdk.SDKMessage` that closes when the CLI exits or errors.
   g. Consumes the channel: each `SDKMessage` is serialized via `runner.events.ToEventPayload` and inserted into `agent_session_events`, then broadcast through `sse.Broadcast` so the still-streaming SSE response to agentserver sees it.
   h. When a `result` message arrives or the upstream SSE cancels, `client.Close` is called and a background goroutine runs `workspace.Teardown`.
4. **workspace.Teardown**: `DiffSnapshot` reads every file that changed since Setup (MEMORY.md written by CC, any new Skills, the updated session `.jsonl`). Each changed file is uploaded through the OpenViking client. The temp dir is removed.
5. **agentserver** drains the SSE, extracts the final assistant text, calls imbridge to send the reply to WeChat.

Tool calls during step 3g happen in-process: when CC emits a `tool_use` for e.g. `mcp__cc-broker__remote_bash`, the SDK's control protocol routes it to the registered Go handler, which calls executor-registry's `/api/execute`. The CLI receives the result through the same stdio channel — no HTTP MCP server, no port assignments, no MCP config JSON file.

## 5. Error Handling

| Failure | Response | Notes |
|---|---|---|
| `workspace.Setup` OpenViking list returns 404 (new workspace) | Succeed with empty dirs | `DownloadTree` already handles this |
| `workspace.Setup` download partial failure | Log warn, continue | Tree may be degraded; CC proceeds with what it has |
| `workspace.Setup` catastrophic failure (mkdir, permissions) | `handler_turns` returns 500 | TurnLock released via defer |
| `runner.Run` CLI spawn fails (binary missing, auth missing) | `handler_turns` returns 500 | Teardown still runs |
| Mid-turn CLI crash | Partial events already broadcast; remaining messages lost; no reply to user | Next turn can resume from whatever the CLI had written to `.jsonl` before crash |
| Tool handler returns error | SDK delivers an error `McpToolResult` to CC; LLM decides how to respond | Not a broker-fatal condition |
| Tool handler panics | SDK recovers; error result to CC; stack logged to cc-broker stderr | Same as above |
| `workspace.Teardown` upload failure | Log warn with the file path; turn reply is already sent; subsequent turn will see slightly stale context | Preserves fail-open behaviour of current worker.go |
| agentserver SSE consumer disconnects mid-turn | Kill CLI via `client.Close`; Teardown continues in background | Matches current worker behaviour |
| `MaxTurns` exceeded inside CC | SDK emits `result` with "max_turns_reached"; normal termination | No special handling required |
| OpenViking tenant auth 400 (`X-OpenViking-Account` missing) | Log warn per file; turn still completes | Pre-existing issue, tracked separately; worsens context drift over time but doesn't break current turn |

No new error paths are introduced by the rewrite. All prior bridge-layer error modes (epoch mismatch, SSE replay limit, stale worker heartbeats, JWT verification) are gone with the deleted handlers.

## 6. Testing

| Layer | Form | Location |
|---|---|---|
| `workspace/` Setup/Teardown | httptest mock OpenViking; assert tree download + snapshot + diff + upload | `workspace/workspace_test.go` |
| `workspace/snapshot` | Pure filesystem unit test against a temp dir; add/modify/remove files; assert diff result | `workspace/snapshot_test.go` |
| `runner/options` | Given a `Workspace` + `AgentSession` + config, assert the `[]QueryOption` contains the expected fields (Resume, Cwd, Env keys, AllowedTools, PermissionMode, Mcp server name) | `runner/options_test.go` |
| `runner/events` | Golden tests: each `SDKMessage` subtype round-trips through `ToEventPayload` to the expected payload shape | `runner/events_test.go` |
| `tools/executor` | Mock executor-registry HTTP; invoke handler with typed input; assert request body + result handling | `tools/executor_test.go` |
| `tools/workspace` | Handler reads/writes in a tempdir that `Context.ClaudeDir` points at; assert file IO | `tools/workspace_test.go` |
| `tools/im` | Mock agentserver HTTP (or its im-inbound endpoint); assert the right shape is POSTed | `tools/im_test.go` |
| `tools/askuser` | Mock agentserver pending-question queue; assert blocking + resolution | `tools/askuser_test.go` |
| `tools/scheduler` | Skipped at this stage (agentserver-side scheduler not implemented; handler stubs return a clear "not implemented" error) | — |
| `handler_turns` orchestration | Fake `workspace` + fake `runner` that emits a scripted sequence of `SDKMessage`; assert TurnLock, DB writes, SSE broadcasts, `done` sentinel | `handler_turns_test.go` |
| End-to-end with real `claude` CLI | Deferred to manual post-deploy smoke test | — |

This is a step up in testability vs the pre-rewrite tree — previously `internal/ccbroker/` had a single `*_test.go` that spun up HTTP servers against the bridge. With the new boundaries, each subpackage can be unit-tested without wiring HTTP fakes.

## 7. Migration & Compatibility

- **External API**: `POST /api/turns` request body, SSE response format, and event payload shape are unchanged. agentserver and imbridge need no code changes.
- **Database**: No schema migrations. `agent_sessions` and `agent_session_events` continue to be populated on every turn; their role narrows from "history replay source for CC" to "audit + frontend SSE source".
- **Deployment**: Single-container change. `kubectl rollout restart deploy/agentserver-ccbroker` after the new `cc-broker:main` image builds. No chart bump required (unless a helm value change is bundled in the same PR).
- **Rollback**: `git revert` the rewrite PR → rebuild `cc-broker:main` → `kubectl rollout restart`. No DB or OpenViking state to roll back.
- **Ordering**: Nothing else in the stack depends on the worker internals. Deploy alone.
- **OpenViking tenant auth**: The `X-OpenViking-Account` / `X-OpenViking-User` problem is independent of this rewrite. Recommended to fix it **before** merging this PR so first real turns can persist their session `.jsonl` back. If the OpenViking fix is delayed, the new cc-broker still works within a single turn (CLI session file is written to the local temp dir, used immediately, then fails to upload) but loses history persistence between turns.
- **SDK default assumptions this spec relies on**:
  - `persistSession` is left at its default (`true`). This is load-bearing — without it the CLI would skip writing `~/.claude/projects/<proj_hash>/<sessionID>.jsonl`, the diff-upload would have nothing to send to OpenViking, and the next turn's `WithResume(sessionID)` would find no file and start from scratch. The Go SDK option `WithPersistSession(false)` **must not be called**.
  - `settingSources` is left at its default (load user + project + local). The `settings.json` we download from OpenViking into `ws.ClaudeDir` (and the `CLAUDE.md` in `ws.ProjectDir`) only take effect when the CLI loads them, which happens under the default setting. Calling `WithSettingSources()` with a filtered subset would silently strip the workspace's configured preferences.

## 8. Open Risks

1. **`WithResume(sessionID)` binds us to the CLI session file format.** If Anthropic changes the `.jsonl` schema in a future CLI release, the resumed conversation context could corrupt or become unreadable. Mitigation: pin `claude` CLI version in the cc-broker Dockerfile (today it auto-upgrades through the install script).
2. **Session file grows unboundedly.** The CLI appends to `<sessionID>.jsonl` every turn; no compaction inside the file. Over long-lived sessions, Setup/Teardown bandwidth grows linearly with conversation length. Mitigation: CC's built-in auto-compaction remains active via `CLAUDE_CODE_AUTO_COMPACT_WINDOW=165000`; compaction writes a summary into the same file, bounding practical size.
3. **In-process tool handlers block the worker goroutine.** A slow `remote_bash` on an offline executor could keep the CLI waiting. Mitigation: every tool handler enforces an executor-registry timeout (existing), and the overall turn is wall-clock-bounded by cc-broker's existing worker max-duration health loop (to be added in a follow-up if not already present).
4. **SDK control-protocol compatibility drift.** `claude-agent-sdk-go` tracks Claude CLI's internal stdio protocol; a CLI update could break framing. Mitigation: SDK is vendored as a module dependency (already the case), and the SDK repo has its own tests for the protocol. CI should include `go test ./...` in cc-broker; if it breaks, the SDK version gets pinned.
