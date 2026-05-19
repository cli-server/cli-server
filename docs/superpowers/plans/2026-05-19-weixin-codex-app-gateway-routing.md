# WeChat → codex-app-gateway Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route WeChat channels with `routing_mode="codex"` through a new `POST /api/turns` REST endpoint on codex-app-gateway (CXG), which thinly converts REST into codex v2 JSON-RPC over a loopback WebSocket and returns the codex `Turn` object verbatim.

**Architecture:** Three components. (1) CXG gains `/api/turns` — a stateless REST↔ws format converter holding a per-workspace loopback ws pool, replying immediately to any codex approval frame, returning the embedded `Turn` on success or `transport:{code,...}` on broker-level failure. (2) agentserver gains a `codex_im_inbound` handler with an in-process per-(channel,user) FIFO dispatcher, persisting `codex_thread_id` to `agent_sessions`, mapping `turn.status`/`turn.error.codexErrorInfo` to user-facing WeChat messages. (3) imbridge bridge adds `case "codex"` in `forwardMessage` and a new `forwardToCodex` HTTP call mirroring `forwardToAgentserver`.

**Tech Stack:** Go 1.22+, `nhooyr.io/websocket`, `github.com/go-chi/chi/v5`, agentserver's existing DB (sqlite migrations 015+), `X-Internal-Secret` shared between agentserver and CXG.

**Spec:** `docs/superpowers/specs/2026-05-19-weixin-codex-app-gateway-routing.md`

---

## File Structure

### CXG (`internal/codexappgateway/`)

| Path | Status | Responsibility |
|---|---|---|
| `broker/protocol.go` | NEW | JSON-RPC envelope + minimum codex v2 type subset (TurnStartParams, TurnStartResponse, TurnCompletedNotification, ItemCompletedNotification, ThreadStartParams/Response, the 4 approval RPCs). Extra fields use `json.RawMessage` for passthrough. |
| `broker/conn.go` | NEW | One loopback ws connection. Lifecycle: dial, initialize/initialized handshake, read loop with notification demux keyed by `turnId`, write side serializing JSON-RPC requests, auto-replying approve/allow to any `*requestApproval`/`requestUserInput`/`elicitation/request` frame, exposing `Turn(ctx, threadID, params, timeoutMs)` and `StartThread(ctx, params)` to callers. |
| `broker/pool.go` | NEW | `map[workspaceID]*Conn` cache, single-flight dial via per-key mutex, idle reap after 5min. Uses `supervisor.EnsureSubprocess` to get the loopback URL. |
| `turn_api.go` | NEW | `POST /api/turns` handler: validates body, dispatches to pool, packs `Turn` or `transport` into REST response. |
| `server.go:248-251` | MODIFY | Mount `/api/turns` with X-Internal-Secret middleware. |
| `codexhome/codexhome.go` | MODIFY | Add `default_tools_approval_mode = "approve"` under `[mcp_servers.agentserver]`. |

### agentserver

| Path | Status | Responsibility |
|---|---|---|
| `internal/db/migrations/024_codex_thread_id.sql` | NEW | `ALTER TABLE agent_sessions ADD COLUMN codex_thread_id TEXT NULL;` |
| `internal/db/agent_sessions.go` | MODIFY | `AgentSession` struct add `CodexThreadID *string`; SELECT columns; new `SetSessionCodexThreadID(ctx, sessionID, threadID *string)`. |
| `internal/server/codex_client.go` | NEW | HTTP client to CXG's `POST /api/turns`. Request/response structs use Go camelCase tags. |
| `internal/server/codex_im_inbound.go` | NEW | `handleCodexIMInbound` (POST `/api/internal/imbridge/codex/turn`): enqueues into per-(channel,user) FIFO dispatcher; worker resolves thread, calls CXG, decodes Turn, POSTs to `/api/internal/imbridge/send`. |
| `internal/server/server.go` | MODIFY | Mount `/api/internal/imbridge/codex/turn`. |
| `internal/imbridge/bridge.go` | MODIFY | Add `case "codex"` in `forwardMessage`; new `forwardToCodex` mirroring `forwardToAgentserver`. |
| `internal/imbridgesvc/handlers.go` | MODIFY | Add `"codex"` to `routing_mode` whitelist. |

---

## Task 1: Add `default_tools_approval_mode = "approve"` to CXG codex config

**Files:**
- Modify: `internal/codexappgateway/codexhome/codexhome.go`
- Test: `internal/codexappgateway/codexhome/codexhome_test.go`

- [ ] **Step 1: Read the current emitter to find where the agentserver MCP block is rendered**

Run: `grep -nE "mcp_servers|agentserver" internal/codexappgateway/codexhome/codexhome.go`
Locate the function that writes the `[mcp_servers.agentserver]` TOML block.

- [ ] **Step 2: Write a failing test**

Add to `internal/codexappgateway/codexhome/codexhome_test.go` (create if missing):

```go
package codexhome

import (
	"strings"
	"testing"
)

func TestWriteConfigEmitsDefaultToolsApprovalMode(t *testing.T) {
	// Read existing ConfigInput shape first:
	//   grep -nA40 "type ConfigInput" internal/codexappgateway/codexhome/codexhome.go
	// Then copy a minimal-but-valid ConfigInput fixture from an existing
	// test in this package (e.g. codexhome_test.go) — required field set
	// varies across releases.
	input := ConfigInput{
		WorkspaceID: "ws-test",
		// add any other REQUIRED fields the existing tests in this file
		// populate to make RenderConfigTOML succeed
	}
	out, err := RenderConfigTOML(input)
	if err != nil {
		t.Fatalf("RenderConfigTOML: %v", err)
	}
	if !strings.Contains(out, `default_tools_approval_mode = "approve"`) {
		t.Errorf("missing default_tools_approval_mode in agentserver MCP block:\n%s", out)
	}
}
```

- [ ] **Step 3: Run test — expect failure**

Run: `go test ./internal/codexappgateway/codexhome/ -run TestWriteConfigEmitsDefaultToolsApprovalMode -v`
Expected: `FAIL` — string not present in output.

- [ ] **Step 4: Add the line to the agentserver block emitter**

In the function that emits `[mcp_servers.agentserver]`, add (after existing fields, before block close):

```go
// Auto-approve all envmcp tool calls. Codex defaults to "auto" with
// approval_required=true for tools lacking readOnlyHint annotations,
// which would surface every read_file/exec_command as a client
// approval prompt. We route over WeChat / REST where interactive
// approval is impossible; the broker also tolerantly approves any
// approval frame that slips through (defense in depth).
fmt.Fprintln(w, `default_tools_approval_mode = "approve"`)
```

(`w` here is whatever buffer/writer the existing block writes to. Adapt if the file uses a templating pattern.)

- [ ] **Step 5: Run test — expect pass**

Run: `go test ./internal/codexappgateway/codexhome/ -run TestWriteConfigEmitsDefaultToolsApprovalMode -v`
Expected: `PASS`.

- [ ] **Step 6: Run full codexhome tests**

Run: `go test ./internal/codexappgateway/codexhome/...`
Expected: all PASS — no other tests broken.

- [ ] **Step 7: Commit**

```bash
git add internal/codexappgateway/codexhome/
git commit -m "feat(codexhome): auto-approve envmcp tool calls

Adds default_tools_approval_mode = \"approve\" to the agentserver
MCP block in the codex config.toml emitter. The new /api/turns REST
path serves channels (e.g. WeChat) where interactive approval is
impossible; the broker also auto-approves any approval frame
defensively, but configuring codex to skip the prompts avoids wasted
roundtrips and keeps oplog cleaner."
```

---

## Task 2: CXG broker — protocol type subset

**Files:**
- Create: `internal/codexappgateway/broker/protocol.go`
- Test: `internal/codexappgateway/broker/protocol_test.go`

This defines just enough codex v2 JSON-RPC types for our REST path. Everything we don't actively read uses `json.RawMessage` for passthrough.

- [ ] **Step 1: Create `protocol.go` skeleton**

```go
// Package broker is a thin REST→ws codex v2 JSON-RPC adapter inside CXG.
// It owns no business logic: it converts a single /api/turns REST call
// into a turn lifecycle on a loopback ws to a codex app-server
// subprocess, returning the resulting codex Turn object verbatim.
package broker

import (
	"encoding/json"
)

// --- JSON-RPC envelopes (codex uses 2.0 but tolerates omission) ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc,omitempty"` // "2.0" — codex tolerates omission, we include
	ID      *int64          `json:"id,omitempty"`      // nil = notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	// Notification methods (ID nil) carry Method + Params instead.
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// --- thread/* (we only construct minimal payloads) ---

// ThreadStartParams: empty {} suffices for MVP — codex defaults the rest.
// All fields optional per v2/thread.rs:ThreadStartParams.
type threadStartParams struct{}

// ThreadStartResponse: we only need thread.id.
type threadStartResponse struct {
	Thread thread `json:"thread"`
}

type thread struct {
	ID string `json:"id"`
}

// --- turn/start ---

// TurnStartParams. We pass through caller's input verbatim via RawMessage
// so codex schema growth (model overrides, environments) doesn't require
// changes here. ThreadID is set by us, not the caller.
type turnStartParams struct {
	ThreadID string          `json:"threadId"`
	Input    json.RawMessage `json:"input"`
	// CallerExtra is merged in at marshal time (see merge helper) when
	// the REST caller passed extra fields beyond `input`. MVP only sends
	// `input`; this is here for future-proofing.
}

// TurnStartResponse: codex returns {turn: Turn}. We need turn.id to
// match later TurnCompleted notifications.
type turnStartResponse struct {
	Turn turnRef `json:"turn"`
}

type turnRef struct {
	ID string `json:"id"`
}

// --- turn/interrupt ---

type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// --- Notifications we listen for ---

// TurnCompletedNotification.params shape (v2/turn.rs:329).
// The full Turn object is opaque to us — we hand it back to the REST
// caller as-is. We only peek threadId/turn.id for routing.
type turnCompletedParams struct {
	ThreadID string          `json:"threadId"`
	Turn     turnPayload     `json:"turn"`
}

// turnPayload exposes only the routing key. The full object is in Raw
// for verbatim REST passthrough.
type turnPayload struct {
	ID  string          `json:"id"`
	Raw json.RawMessage `json:"-"` // populated by custom UnmarshalJSON
}

func (t *turnPayload) UnmarshalJSON(data []byte) error {
	t.Raw = append(t.Raw[:0], data...)
	var shell struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &shell); err != nil {
		return err
	}
	t.ID = shell.ID
	return nil
}

// --- Approval frames we auto-reply to ---

// Methods listed at codex-rs/app-server-protocol/src/protocol/common.rs:1277.
// We don't unmarshal params; we just need the request id to reply.
const (
	methodItemCmdApproval     = "item/commandExecution/requestApproval"
	methodItemFileApproval    = "item/fileChange/requestApproval"
	methodItemUserInput       = "item/tool/requestUserInput"
	methodItemPermsApproval   = "item/permissions/requestApproval"
	methodMcpElicitation      = "mcpServer/elicitation/request"
)

func isApprovalRequest(method string) bool {
	switch method {
	case methodItemCmdApproval, methodItemFileApproval, methodItemUserInput,
		methodItemPermsApproval, methodMcpElicitation:
		return true
	}
	return false
}

// approvalReply returns the JSON we send back. Codex's decision enums
// differ per request type but all accept "approve"/"allow" shapes —
// per CommandExecutionApprovalDecision, FileChangeApprovalDecision,
// PermissionsApprovalDecision (all carry "approve" or equivalent).
// requestUserInput is generic; codex accepts {} or {"decision":"allow"}.
func approvalReply(method string) json.RawMessage {
	switch method {
	case methodItemPermsApproval:
		return json.RawMessage(`{"decision":"allow"}`)
	default:
		return json.RawMessage(`{"decision":"approve"}`)
	}
}
```

- [ ] **Step 2: Write tests for envelope round-trip and approval detection**

Create `internal/codexappgateway/broker/protocol_test.go`:

```go
package broker

import (
	"encoding/json"
	"testing"
)

func TestRPCRequestMarshalsNotification(t *testing.T) {
	r := rpcRequest{
		JSONRPC: "2.0",
		Method:  "initialized",
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"jsonrpc":"2.0","method":"initialized"}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestRPCRequestMarshalsCall(t *testing.T) {
	id := int64(7)
	r := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "thread/start",
		Params:  json.RawMessage(`{}`),
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"jsonrpc":"2.0","id":7,"method":"thread/start","params":{}}` {
		t.Errorf("got %s", b)
	}
}

func TestTurnCompletedParamsKeepsRawTurn(t *testing.T) {
	frame := []byte(`{"threadId":"thr-1","turn":{"id":"trn-9","status":"completed","items":[{"type":"agentMessage","id":"msg-1","text":"hi"}],"itemsView":"full","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}`)
	var p turnCompletedParams
	if err := json.Unmarshal(frame, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ThreadID != "thr-1" {
		t.Errorf("threadID=%q", p.ThreadID)
	}
	if p.Turn.ID != "trn-9" {
		t.Errorf("turn.id=%q", p.Turn.ID)
	}
	// The raw Turn payload must be preserved verbatim for REST passthrough.
	var rt map[string]any
	if err := json.Unmarshal(p.Turn.Raw, &rt); err != nil {
		t.Fatalf("turn.raw unmarshal: %v", err)
	}
	if rt["status"] != "completed" {
		t.Errorf("raw turn lost: %v", rt)
	}
	items, _ := rt["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items lost: %v", items)
	}
}

func TestIsApprovalRequest(t *testing.T) {
	cases := map[string]bool{
		"item/commandExecution/requestApproval": true,
		"item/fileChange/requestApproval":       true,
		"item/tool/requestUserInput":            true,
		"item/permissions/requestApproval":      true,
		"mcpServer/elicitation/request":         true,
		"turn/started":                          false,
		"turn/completed":                        false,
		"item/completed":                        false,
	}
	for m, want := range cases {
		if got := isApprovalRequest(m); got != want {
			t.Errorf("%s: got %v want %v", m, got, want)
		}
	}
}

func TestApprovalReplyForPermsUsesAllow(t *testing.T) {
	got := string(approvalReply(methodItemPermsApproval))
	if got != `{"decision":"allow"}` {
		t.Errorf("perms reply = %s", got)
	}
}

func TestApprovalReplyDefaultsToApprove(t *testing.T) {
	got := string(approvalReply(methodItemCmdApproval))
	if got != `{"decision":"approve"}` {
		t.Errorf("cmd reply = %s", got)
	}
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/codexappgateway/broker/ -v`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/codexappgateway/broker/
git commit -m "feat(broker): codex v2 JSON-RPC type subset

Minimal type set needed for the /api/turns REST path. Envelope
(rpcRequest/rpcResponse/rpcError) + thread/start + turn/start +
turn/interrupt request shapes + TurnCompleted notification with
verbatim raw-turn passthrough + approval RPC detection and reply
helpers. Everything we do not actively read uses json.RawMessage."
```

---

## Task 3: CXG broker — `Conn` initialize/initialized handshake

**Files:**
- Create: `internal/codexappgateway/broker/conn.go`
- Test: `internal/codexappgateway/broker/conn_handshake_test.go`

- [ ] **Step 1: Write failing test (mock codex over ws) for the handshake**

Create `internal/codexappgateway/broker/conn_handshake_test.go`:

```go
package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// fakeCodexServer accepts one ws connection and runs `frame` against it.
// frame receives Read/Write helpers and must replay codex behavior.
func fakeCodexServer(t *testing.T, frame func(t *testing.T, ctx context.Context, c *websocket.Conn)) (wsURL string, stop func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Logf("accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		frame(t, r.Context(), c)
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	return url, srv.Close
}

func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func writeJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestConnInitializeHandshake(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		init := readFrame(t, ctx, c)
		if init["method"] != "initialize" {
			t.Errorf("first frame method=%v want initialize", init["method"])
		}
		// Reply with initialize result.
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"id":      init["id"],
			"result":  map[string]any{"protocolVersion": "2025-06-18"},
		})
		// Expect initialized notification.
		got := readFrame(t, ctx, c)
		if got["method"] != "initialized" {
			t.Errorf("second frame method=%v want initialized", got["method"])
		}
		if _, hasID := got["id"]; hasID {
			t.Errorf("initialized must be a notification (no id), got %v", got)
		}
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialAndHandshake(ctx, url)
	if err != nil {
		t.Fatalf("dialAndHandshake: %v", err)
	}
	defer conn.Close()
}
```

- [ ] **Step 2: Run test — expect compile failure (dialAndHandshake undefined)**

Run: `go test ./internal/codexappgateway/broker/ -run TestConnInitializeHandshake -v`
Expected: `undefined: dialAndHandshake`.

- [ ] **Step 3: Create `conn.go` with minimal Conn type + dialAndHandshake**

```go
package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"nhooyr.io/websocket"
)

// Conn is one loopback ws to a codex app-server subprocess. Safe for
// concurrent Turn() / StartThread() calls — internally serializes
// writes and demuxes notifications by turnId.
type Conn struct {
	ws        *websocket.Conn
	writeMu   sync.Mutex
	nextID    atomic.Int64
	closeOnce sync.Once
	closed    chan struct{}
}

// dialAndHandshake dials wsURL, runs initialize → initialized, returns
// a ready-to-use Conn. Caller must Close() it.
func dialAndHandshake(ctx context.Context, wsURL string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled, // codex rejects permessage-deflate
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	ws.SetReadLimit(64 << 20) // match server-side limit

	c := &Conn{ws: ws, closed: make(chan struct{})}

	// initialize (request)
	initParams := json.RawMessage(`{"clientInfo":{"name":"agentserver-codex-broker","version":"0.1.0"},"protocolVersion":"2025-06-18","capabilities":{}}`)
	if _, err := c.callRaw(ctx, "initialize", initParams); err != nil {
		ws.Close(websocket.StatusInternalError, "")
		return nil, fmt.Errorf("initialize: %w", err)
	}

	// initialized (notification)
	if err := c.notifyRaw(ctx, "initialized", json.RawMessage(`{}`)); err != nil {
		ws.Close(websocket.StatusInternalError, "")
		return nil, fmt.Errorf("initialized: %w", err)
	}
	return c, nil
}

// Close shuts down the ws. Safe to call multiple times.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.ws.Close(websocket.StatusNormalClosure, "")
	})
}

// callRaw sends a JSON-RPC request and synchronously reads frames until
// the matching response is found. THIS IS A STUB for handshake only —
// real call flow goes through the demuxed reader in later tasks.
func (c *Conn) callRaw(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	if err := c.writeJSON(ctx, req); err != nil {
		return nil, err
	}
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		var resp rpcResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if resp.ID != nil && *resp.ID == id {
			if resp.Error != nil {
				return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
		// Drop notifications during handshake; demux comes later.
	}
}

func (c *Conn) notifyRaw(ctx context.Context, method string, params json.RawMessage) error {
	return c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Conn) writeJSON(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, b)
}
```

- [ ] **Step 4: Run test — expect pass**

Run: `go test ./internal/codexappgateway/broker/ -run TestConnInitializeHandshake -v`
Expected: `PASS`.

- [ ] **Step 5: Add `go.mod` dep verification**

Run: `cd /root/agentserver && go build ./internal/codexappgateway/broker/`
Expected: no errors. (`nhooyr.io/websocket` already in go.mod.)

- [ ] **Step 6: Commit**

```bash
git add internal/codexappgateway/broker/conn.go internal/codexappgateway/broker/conn_handshake_test.go
git commit -m "feat(broker): Conn dial + initialize/initialized handshake

Minimum viable Conn for ws lifecycle: dial with permessage-deflate
disabled (codex rejects it), run synchronous initialize request, send
initialized notification. callRaw is a stub that drops notifications —
later tasks replace it with a demuxed reader so concurrent turns work."
```

---

## Task 4: CXG broker — turn lifecycle (turn/start + notification demux + turn/completed)

**Files:**
- Modify: `internal/codexappgateway/broker/conn.go`
- Test: `internal/codexappgateway/broker/conn_turn_test.go`

This replaces the handshake-only stub with a demuxed reader loop. Each turn registers a channel keyed by turn id; the read loop dispatches `turn/completed` notifications to the right channel.

- [ ] **Step 1: Write the turn-lifecycle test**

Create `internal/codexappgateway/broker/conn_turn_test.go`:

```go
package broker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// replayHandshake reads initialize + initialized frames and replies to
// initialize so dialAndHandshake completes. Returns once both are seen.
func replayHandshake(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	init := readFrame(t, ctx, c)
	writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": init["id"], "result": map[string]any{}})
	got := readFrame(t, ctx, c)
	if got["method"] != "initialized" {
		t.Fatalf("expected initialized, got %v", got)
	}
}

func TestConnTurnSuccessful(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)

		// Expect turn/start call.
		ts := readFrame(t, ctx, c)
		if ts["method"] != "turn/start" {
			t.Fatalf("want turn/start, got %v", ts["method"])
		}
		params := ts["params"].(map[string]any)
		if params["threadId"] != "thr-abc" {
			t.Errorf("threadId=%v", params["threadId"])
		}
		// Reply with turn/start response (turn id).
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"id":      ts["id"],
			"result":  map[string]any{"turn": map[string]any{"id": "trn-001"}},
		})

		// Stream notifications then turn/completed.
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/started",
			"params":  map[string]any{"threadId": "thr-abc", "turn": map[string]any{"id": "trn-001"}},
		})
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/completed",
			"params": map[string]any{
				"threadId": "thr-abc",
				"turn": map[string]any{
					"id":          "trn-001",
					"status":      "completed",
					"itemsView":   "full",
					"items":       []any{map[string]any{"type": "agentMessage", "id": "msg1", "text": "hello"}},
					"error":       nil,
					"startedAt":   1,
					"completedAt": 2,
					"durationMs":  1000,
				},
			},
		})
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	rawTurn, err := conn.Turn(ctx, "thr-abc", json.RawMessage(`{"input":[{"type":"text","text":"hi"}]}`), 5*time.Second)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rawTurn, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "completed" {
		t.Errorf("status=%v", got["status"])
	}
	items := got["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["text"] != "hello" {
		t.Errorf("items=%v", items)
	}
}
```

- [ ] **Step 2: Run test — expect failure (Turn / Dial undefined or stub doesn't demux)**

Run: `go test ./internal/codexappgateway/broker/ -run TestConnTurnSuccessful -v`
Expected: FAIL.

- [ ] **Step 3: Rewrite `conn.go` with demuxed reader + Turn API**

Replace `conn.go` with:

```go
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// Conn is one loopback ws to a codex app-server subprocess. Safe for
// concurrent Turn() / StartThread() calls — internally serializes
// writes and demuxes responses + turn/completed notifications.
type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	nextID  atomic.Int64

	mu           sync.Mutex
	pendingResp  map[int64]chan rpcResponse  // request id → 1-buffered chan
	pendingTurns map[string]chan turnPayload // turn id → 1-buffered chan

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  atomic.Value // error, set when reader exits
}

// Dial opens a fresh ws, performs the codex initialize / initialized
// handshake, and starts the reader goroutine. Caller must Close().
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	ws.SetReadLimit(64 << 20)

	c := &Conn{
		ws:           ws,
		pendingResp:  make(map[int64]chan rpcResponse),
		pendingTurns: make(map[string]chan turnPayload),
		closed:       make(chan struct{}),
	}

	// Send initialize synchronously (no reader yet, so we read inline).
	id := c.nextID.Add(1)
	if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "initialize", Params: json.RawMessage(`{"clientInfo":{"name":"agentserver-codex-broker","version":"0.1.0"},"protocolVersion":"2025-06-18","capabilities":{}}`)}); err != nil {
		ws.Close(websocket.StatusInternalError, "")
		return nil, fmt.Errorf("initialize: %w", err)
	}
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			ws.Close(websocket.StatusInternalError, "")
			return nil, fmt.Errorf("initialize read: %w", err)
		}
		var resp rpcResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			ws.Close(websocket.StatusInternalError, "")
			return nil, fmt.Errorf("initialize decode: %w", err)
		}
		if resp.ID != nil && *resp.ID == id {
			if resp.Error != nil {
				ws.Close(websocket.StatusInternalError, "")
				return nil, fmt.Errorf("initialize rpc error: %s", resp.Error.Message)
			}
			break
		}
	}
	// initialized (notification).
	if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", Method: "initialized", Params: json.RawMessage(`{}`)}); err != nil {
		ws.Close(websocket.StatusInternalError, "")
		return nil, fmt.Errorf("initialized: %w", err)
	}

	go c.readLoop()
	return c, nil
}

// readLoop consumes every inbound frame and routes it: rpc responses
// to pendingResp[id]; turn/completed notifications to pendingTurns;
// approval requests get auto-replied; everything else is dropped.
func (c *Conn) readLoop() {
	defer c.failAllPending(errors.New("connection closed"))

	for {
		ctx, cancel := context.WithCancel(context.Background())
		// Tie reader lifecycle to Close.
		go func() { <-c.closed; cancel() }()
		_, data, err := c.ws.Read(ctx)
		cancel()
		if err != nil {
			c.closeErr.Store(err)
			return
		}
		c.dispatchFrame(data)
	}
}

func (c *Conn) dispatchFrame(data []byte) {
	var f rpcResponse // shape covers both response and notification
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	if f.ID != nil && f.Method == "" {
		c.deliverResponse(*f.ID, f)
		return
	}
	// Notification or server request.
	if f.ID != nil && isApprovalRequest(f.Method) {
		// Auto-reply (handled in Task 5; stub here).
		_ = c.writeJSON(context.Background(), rpcResponse{
			JSONRPC: "2.0", ID: f.ID, Result: approvalReply(f.Method),
		})
		return
	}
	if f.Method == "turn/completed" {
		var p turnCompletedParams
		if err := json.Unmarshal(f.Params, &p); err != nil {
			return
		}
		c.deliverTurn(p.Turn.ID, p.Turn)
		return
	}
	// Drop other notifications.
}

func (c *Conn) deliverResponse(id int64, resp rpcResponse) {
	c.mu.Lock()
	ch, ok := c.pendingResp[id]
	delete(c.pendingResp, id)
	c.mu.Unlock()
	if ok {
		ch <- resp
	}
}

func (c *Conn) deliverTurn(turnID string, payload turnPayload) {
	c.mu.Lock()
	ch, ok := c.pendingTurns[turnID]
	delete(c.pendingTurns, turnID)
	c.mu.Unlock()
	if ok {
		ch <- payload
	}
}

func (c *Conn) failAllPending(err error) {
	c.mu.Lock()
	for id, ch := range c.pendingResp {
		close(ch)
		delete(c.pendingResp, id)
	}
	for tid, ch := range c.pendingTurns {
		close(ch)
		delete(c.pendingTurns, tid)
	}
	c.mu.Unlock()
	c.closeErr.Store(err)
}

// Turn sends turn/start and blocks until the matching turn/completed
// notification arrives or timeout elapses. Returns the raw codex Turn
// JSON for verbatim REST passthrough.
func (c *Conn) Turn(ctx context.Context, threadID string, callerParams json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	// Build params: merge {threadId} with caller-supplied input/etc.
	mergedParams, err := mergeTurnParams(threadID, callerParams)
	if err != nil {
		return nil, fmt.Errorf("merge params: %w", err)
	}

	// Pre-register response slot.
	id := c.nextID.Add(1)
	respCh := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pendingResp[id] = respCh
	c.mu.Unlock()

	if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "turn/start", Params: mergedParams}); err != nil {
		c.mu.Lock()
		delete(c.pendingResp, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write turn/start: %w", err)
	}

	// Wait for turn/start response → extract turn id.
	resp, ok := waitResp(ctx, respCh)
	if !ok {
		return nil, c.closeErrOr(errors.New("connection closed before turn/start response"))
	}
	if resp.Error != nil {
		return nil, &TurnRPCError{Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}
	}
	var startResp turnStartResponse
	if err := json.Unmarshal(resp.Result, &startResp); err != nil {
		return nil, fmt.Errorf("decode turn/start result: %w", err)
	}
	if startResp.Turn.ID == "" {
		return nil, fmt.Errorf("turn/start result missing turn.id")
	}

	// Register turn completion slot before waiting.
	turnCh := make(chan turnPayload, 1)
	c.mu.Lock()
	c.pendingTurns[startResp.Turn.ID] = turnCh
	c.mu.Unlock()

	// Wait with timeout.
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case payload, open := <-turnCh:
		if !open {
			return nil, c.closeErrOr(errors.New("connection closed before turn/completed"))
		}
		return payload.Raw, nil
	case <-tctx.Done():
		// Hand back the partial registration cleanup
		c.mu.Lock()
		delete(c.pendingTurns, startResp.Turn.ID)
		c.mu.Unlock()
		return nil, &TimeoutError{ThreadID: threadID, TurnID: startResp.Turn.ID}
	}
}

func waitResp(ctx context.Context, ch chan rpcResponse) (rpcResponse, bool) {
	select {
	case resp, open := <-ch:
		return resp, open
	case <-ctx.Done():
		return rpcResponse{}, false
	}
}

func (c *Conn) closeErrOr(fallback error) error {
	if v := c.closeErr.Load(); v != nil {
		if err, ok := v.(error); ok && err != nil {
			return err
		}
	}
	return fallback
}

// mergeTurnParams takes the caller-supplied params blob (which must be
// a JSON object) and merges {"threadId": threadID} into it without
// overwriting other caller fields. The caller MUST NOT include
// threadId — broker owns thread routing.
func mergeTurnParams(threadID string, caller json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if len(caller) == 0 {
		m = map[string]json.RawMessage{}
	} else if err := json.Unmarshal(caller, &m); err != nil {
		return nil, fmt.Errorf("caller params is not a JSON object: %w", err)
	}
	if _, exists := m["threadId"]; exists {
		return nil, errors.New("caller params must not include threadId")
	}
	tid, _ := json.Marshal(threadID)
	m["threadId"] = tid
	return json.Marshal(m)
}

// TurnRPCError is returned by Turn when codex returns a JSON-RPC error
// in response to turn/start (rare; usually means malformed request).
type TurnRPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *TurnRPCError) Error() string { return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message) }

// TimeoutError is returned when timeoutMs elapses without turn/completed.
type TimeoutError struct {
	ThreadID, TurnID string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("turn timed out (thread=%s turn=%s)", e.ThreadID, e.TurnID)
}

// Close shuts down the ws. Safe to call multiple times.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.ws.Close(websocket.StatusNormalClosure, "")
	})
}

func (c *Conn) writeJSON(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, b)
}
```

- [ ] **Step 4: Delete the obsolete handshake test (now replaced by Dial in turn test)**

Delete `internal/codexappgateway/broker/conn_handshake_test.go` (it covered the old `dialAndHandshake` symbol; `Dial` is exercised in `conn_turn_test.go` indirectly).

```bash
rm internal/codexappgateway/broker/conn_handshake_test.go
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/codexappgateway/broker/ -v`
Expected: `TestConnTurnSuccessful` PASS plus all protocol tests still PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/codexappgateway/broker/
git commit -m "feat(broker): Conn.Turn with demuxed reader + turn/completed wait

Replaces the handshake-only stub with a full ws reader loop:
- inbound frames are demuxed to pendingResp[id] (for request/response)
  or pendingTurns[turnId] (for turn/completed notifications)
- approval frames get auto-replied with approve/allow
- Turn() sends turn/start, waits for its response to learn turn id,
  registers a turn-completion channel, and blocks until that fires or
  the per-call timeout elapses
- Returns the raw codex Turn JSON for REST passthrough
- TurnRPCError + TimeoutError sentinels for caller dispatching"
```

---

## Task 5: CXG broker — verify auto-approval handling end-to-end

The dispatch logic was stubbed in Task 4 (`isApprovalRequest` check in `dispatchFrame`). This task adds a dedicated test so regressions are caught.

**Files:**
- Test: `internal/codexappgateway/broker/conn_approval_test.go`

- [ ] **Step 1: Write test**

```go
package broker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestConnAutoApprovesRequestUserInput(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)

		// turn/start → reply
		ts := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": ts["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-1"}}})

		// Server sends an approval request mid-turn.
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"id":      999,
			"method":  "item/tool/requestUserInput",
			"params":  map[string]any{"toolName": "read_file"},
		})
		// Expect the broker to reply with decision:approve.
		approval := readFrame(t, ctx, c)
		if approval["id"] != float64(999) {
			t.Errorf("approval reply id=%v want 999", approval["id"])
		}
		result := approval["result"].(map[string]any)
		if result["decision"] != "approve" {
			t.Errorf("decision=%v want approve", result["decision"])
		}

		// Finish the turn so Turn() returns.
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/completed",
			"params":  map[string]any{"threadId": "thr-1", "turn": map[string]any{"id": "trn-1", "status": "completed", "items": []any{}, "itemsView": "full", "error": nil}},
		})
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Turn(ctx, "thr-1", json.RawMessage(`{"input":[{"type":"text","text":"hi"}]}`), 5*time.Second); err != nil {
		t.Fatalf("Turn: %v", err)
	}
}

func TestConnAutoApprovesPermissionsWithAllow(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		ts := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": ts["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-2"}}})

		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0", "id": 555,
			"method": "item/permissions/requestApproval",
			"params": map[string]any{},
		})
		reply := readFrame(t, ctx, c)
		if reply["result"].(map[string]any)["decision"] != "allow" {
			t.Errorf("perms decision=%v want allow", reply["result"])
		}
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/completed",
			"params":  map[string]any{"threadId": "thr-2", "turn": map[string]any{"id": "trn-2", "status": "completed", "items": []any{}, "itemsView": "full", "error": nil}},
		})
	})
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Turn(ctx, "thr-2", json.RawMessage(`{"input":[]}`), 5*time.Second); err != nil {
		t.Fatalf("Turn: %v", err)
	}
}
```

- [ ] **Step 2: Run tests — expect PASS (logic already in Task 4)**

Run: `go test ./internal/codexappgateway/broker/ -run TestConnAutoApproves -v`
Expected: both PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/codexappgateway/broker/conn_approval_test.go
git commit -m "test(broker): auto-approval coverage for requestUserInput and permissions

Pins the dispatchFrame behavior added in the previous commit: any
item/*/requestApproval, item/tool/requestUserInput, or
mcpServer/elicitation/request server-side request is replied with
decision=approve (or decision=allow for permissions) immediately,
without surfacing to the REST caller."
```

---

## Task 6: CXG broker — `StartThread` helper for null-threadId case

`Turn` requires a thread id. When the REST caller passes `threadId: null`, the broker must first call `thread/start`.

**Files:**
- Modify: `internal/codexappgateway/broker/conn.go`
- Test: `internal/codexappgateway/broker/conn_thread_test.go`

- [ ] **Step 1: Write test**

```go
package broker

import (
	"context"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestConnStartThread(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		req := readFrame(t, ctx, c)
		if req["method"] != "thread/start" {
			t.Fatalf("method=%v want thread/start", req["method"])
		}
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result": map[string]any{
				"thread":         map[string]any{"id": "thr-new", "sessionId": "sess", "createdAt": 0, "updatedAt": 0},
				"model":          "gpt-x",
				"modelProvider":  "openai",
				"serviceTier":    nil,
				"cwd":            "/tmp/codex",
				"approvalPolicy": "onRequest",
			},
		})
	})
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	id, err := conn.StartThread(ctx)
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	if id != "thr-new" {
		t.Errorf("thread id=%q want thr-new", id)
	}
}
```

- [ ] **Step 2: Run — expect compile failure**

Run: `go test ./internal/codexappgateway/broker/ -run TestConnStartThread -v`
Expected: `undefined: (*Conn).StartThread`.

- [ ] **Step 3: Add `StartThread` to `conn.go`**

Append to `conn.go`:

```go
// StartThread issues thread/start with empty params and returns the new
// thread id. Other ThreadStartResponse fields are discarded — CXG only
// owns the loopback, agentserver tracks per-conversation state.
func (c *Conn) StartThread(ctx context.Context) (string, error) {
	id := c.nextID.Add(1)
	respCh := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pendingResp[id] = respCh
	c.mu.Unlock()

	if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "thread/start", Params: json.RawMessage(`{}`)}); err != nil {
		c.mu.Lock()
		delete(c.pendingResp, id)
		c.mu.Unlock()
		return "", fmt.Errorf("write thread/start: %w", err)
	}
	resp, ok := waitResp(ctx, respCh)
	if !ok {
		return "", c.closeErrOr(errors.New("connection closed before thread/start response"))
	}
	if resp.Error != nil {
		return "", &TurnRPCError{Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}
	}
	var tsResp threadStartResponse
	if err := json.Unmarshal(resp.Result, &tsResp); err != nil {
		return "", fmt.Errorf("decode thread/start: %w", err)
	}
	if tsResp.Thread.ID == "" {
		return "", errors.New("thread/start result missing thread.id")
	}
	return tsResp.Thread.ID, nil
}
```

- [ ] **Step 4: Run — expect pass**

Run: `go test ./internal/codexappgateway/broker/ -run TestConnStartThread -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/broker/
git commit -m "feat(broker): Conn.StartThread for null-threadId REST calls

Sends thread/start with empty params and extracts thread.id from the
response. Other ThreadStartResponse fields (model, cwd, etc.) are
discarded since broker owns no per-thread state."
```

---

## Task 7: CXG broker — `Conn.Interrupt` (for timeout cleanup)

When a `Turn()` call hits its per-call timeout, broker should send `turn/interrupt` so codex doesn't keep working. The timeout itself returns `TimeoutError` already; this task adds the interrupt-emit + a wsDisconnect propagation test.

**Files:**
- Modify: `internal/codexappgateway/broker/conn.go`
- Test: `internal/codexappgateway/broker/conn_interrupt_test.go`

- [ ] **Step 1: Write tests for interrupt-on-timeout + wsDisconnect**

```go
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestConnTurnInterruptOnTimeout(t *testing.T) {
	gotInterrupt := make(chan map[string]any, 1)
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		ts := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": ts["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-late"}}})
		// Never send turn/completed; wait for interrupt.
		for {
			f := readFrame(t, ctx, c)
			if f["method"] == "turn/interrupt" {
				gotInterrupt <- f
				return
			}
		}
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	_, err = conn.Turn(ctx, "thr-late", json.RawMessage(`{"input":[]}`), 200*time.Millisecond)
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v want *TimeoutError", err)
	}

	select {
	case f := <-gotInterrupt:
		p := f["params"].(map[string]any)
		if p["threadId"] != "thr-late" || p["turnId"] != "trn-late" {
			t.Errorf("interrupt params = %v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not observe turn/interrupt within 3s after timeout")
	}
}

func TestConnTurnFailsOnWSClose(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		ts := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": ts["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-x"}}})
		// Close ws mid-turn instead of sending turn/completed.
		c.Close(websocket.StatusInternalError, "simulated crash")
	})
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_, err = conn.Turn(ctx, "thr-x", json.RawMessage(`{"input":[]}`), 5*time.Second)
	if err == nil {
		t.Fatal("expected error on ws close")
	}
	// Either an explicit timeout (race) or close error is acceptable; just
	// ensure it doesn't deadlock.
	t.Logf("ws-close err = %v", err)
}
```

- [ ] **Step 2: Run — expect first test to FAIL (no interrupt sent)**

Run: `go test ./internal/codexappgateway/broker/ -run TestConnTurn -v`
Expected: `TestConnTurnInterruptOnTimeout` FAILS with "did not observe turn/interrupt".

- [ ] **Step 3: Add interrupt-on-timeout to `Turn`**

In `conn.go`, locate the `case <-tctx.Done():` branch inside `Turn` and modify:

```go
		case <-tctx.Done():
			c.mu.Lock()
			delete(c.pendingTurns, startResp.Turn.ID)
			c.mu.Unlock()
			// Best-effort interrupt so codex doesn't keep working.
			bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			ipB, _ := json.Marshal(turnInterruptParams{ThreadID: threadID, TurnID: startResp.Turn.ID})
			interruptID := c.nextID.Add(1)
			_ = c.writeJSON(bgCtx, rpcRequest{
				JSONRPC: "2.0", ID: &interruptID, Method: "turn/interrupt", Params: ipB,
			})
			cancel()
			return nil, &TimeoutError{ThreadID: threadID, TurnID: startResp.Turn.ID}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/codexappgateway/broker/ -run TestConnTurn -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/broker/
git commit -m "feat(broker): emit turn/interrupt on Turn timeout

When a Turn call hits its per-call timeout, fire-and-forget a
turn/interrupt to codex so the subprocess does not keep working on a
turn nobody is waiting for. The interrupt is best-effort (2s timeout
on a background context); the TimeoutError is returned regardless.

Adds ws-close passthrough coverage so a mid-turn subprocess crash
returns an error promptly rather than deadlocking."
```

---

## Task 8: CXG broker — per-workspace `Pool` with idle reap

**Files:**
- Create: `internal/codexappgateway/broker/pool.go`
- Test: `internal/codexappgateway/broker/pool_test.go`

The pool caches one `*Conn` per workspace and reaps connections idle >5min.

- [ ] **Step 1: Write pool test**

```go
package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// countingCodexServer counts how many ws Accepts happen so we can
// verify pool reuse vs fresh dial.
func countingCodexServer(t *testing.T) (urlFn func(workspaceID string) string, dialCount *atomic.Int64, stop func()) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		// handshake
		ctx := r.Context()
		init := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": init["id"], "result": map[string]any{}})
		readFrame(t, ctx, c) // initialized
		// echo each turn
		for {
			f, err := readNoFatal(ctx, c)
			if err != nil {
				return
			}
			if f["method"] == "turn/start" {
				writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": f["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-pool"}}})
				writeJSON(t, ctx, c, map[string]any{
					"jsonrpc": "2.0",
					"method":  "turn/completed",
					"params":  map[string]any{"threadId": "thr-x", "turn": map[string]any{"id": "trn-pool", "status": "completed", "items": []any{}, "itemsView": "full", "error": nil}},
				})
			}
		}
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	return func(string) string { return url }, &count, srv.Close
}

func readNoFatal(ctx context.Context, c *websocket.Conn) (map[string]any, error) {
	_, data, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func TestPoolReusesConnForSameWorkspace(t *testing.T) {
	urlFn, dialCount, stop := countingCodexServer(t)
	defer stop()

	resolver := func(ctx context.Context, workspaceID string) (string, error) {
		return urlFn(workspaceID), nil
	}
	p := NewPool(resolver, 5*time.Minute)
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		conn, err := p.Get(ctx, "ws-A")
		if err != nil {
			t.Fatalf("iter %d Get: %v", i, err)
		}
		if _, err := conn.Turn(ctx, "thr-x", json.RawMessage(`{"input":[]}`), 5*time.Second); err != nil {
			t.Fatalf("iter %d Turn: %v", i, err)
		}
	}
	if dialCount.Load() != 1 {
		t.Errorf("dialCount=%d want 1 (pool should reuse)", dialCount.Load())
	}
}

func TestPoolReapsIdleConn(t *testing.T) {
	urlFn, dialCount, stop := countingCodexServer(t)
	defer stop()

	resolver := func(ctx context.Context, _ string) (string, error) { return urlFn(""), nil }
	p := NewPool(resolver, 100*time.Millisecond)
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := p.Get(ctx, "ws-A")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := conn.Turn(ctx, "thr-x", json.RawMessage(`{"input":[]}`), 5*time.Second); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	// Wait past the idle TTL plus the reaper's tick interval.
	time.Sleep(400 * time.Millisecond)

	conn2, err := p.Get(ctx, "ws-A")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if conn2 == conn {
		t.Error("expected fresh Conn after idle reap")
	}
	if dialCount.Load() != 2 {
		t.Errorf("dialCount=%d want 2 (idle reap should force redial)", dialCount.Load())
	}
}
```

- [ ] **Step 2: Run — expect compile failure**

Run: `go test ./internal/codexappgateway/broker/ -run TestPool -v`
Expected: `undefined: NewPool, Pool`.

- [ ] **Step 3: Create `pool.go`**

```go
package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// WSURLResolver is the per-workspace loopback ws URL provider. In
// production this calls into supervisor.EnsureSubprocess to spawn /
// reuse the codex subprocess; tests inject a fixed URL.
type WSURLResolver func(ctx context.Context, workspaceID string) (wsURL string, err error)

// Pool caches one *Conn per workspace id. Connections idle for longer
// than idleTTL are reaped and closed. Safe for concurrent use.
type Pool struct {
	resolver WSURLResolver
	idleTTL  time.Duration

	mu      sync.Mutex
	entries map[string]*poolEntry
	stop    chan struct{}
}

type poolEntry struct {
	mu         sync.Mutex // single-flight Dial per workspace
	conn       *Conn
	lastUsedAt time.Time
}

// NewPool starts a background reaper goroutine. Caller must Close().
func NewPool(resolver WSURLResolver, idleTTL time.Duration) *Pool {
	p := &Pool{
		resolver: resolver,
		idleTTL:  idleTTL,
		entries:  make(map[string]*poolEntry),
		stop:     make(chan struct{}),
	}
	go p.reaper()
	return p
}

// Get returns a live *Conn for workspaceID, dialing if necessary.
// Concurrent Get calls for the same workspace share one Conn.
func (p *Pool) Get(ctx context.Context, workspaceID string) (*Conn, error) {
	if workspaceID == "" {
		return nil, errors.New("workspaceID required")
	}
	p.mu.Lock()
	e, ok := p.entries[workspaceID]
	if !ok {
		e = &poolEntry{}
		p.entries[workspaceID] = e
	}
	p.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.conn != nil && !e.connClosed() {
		e.lastUsedAt = time.Now()
		return e.conn, nil
	}
	// (Re)dial.
	wsURL, err := p.resolver(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve loopback url: %w", err)
	}
	conn, err := Dial(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	e.conn = conn
	e.lastUsedAt = time.Now()
	return conn, nil
}

func (e *poolEntry) connClosed() bool {
	if e.conn == nil {
		return true
	}
	// closeErr non-nil → reader has exited; the ws is dead.
	if v := e.conn.closeErr.Load(); v != nil {
		if err, _ := v.(error); err != nil {
			return true
		}
	}
	return false
}

// Close stops the reaper and closes all live connections.
func (p *Pool) Close() {
	close(p.stop)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		e.mu.Lock()
		if e.conn != nil {
			e.conn.Close()
			e.conn = nil
		}
		e.mu.Unlock()
	}
	p.entries = map[string]*poolEntry{}
}

func (p *Pool) reaper() {
	tick := time.NewTicker(p.idleTTL / 4)
	if p.idleTTL/4 < 50*time.Millisecond {
		tick = time.NewTicker(50 * time.Millisecond)
	}
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-tick.C:
			p.reapOnce()
		}
	}
}

func (p *Pool) reapOnce() {
	cutoff := time.Now().Add(-p.idleTTL)
	p.mu.Lock()
	keys := make([]string, 0, len(p.entries))
	for k := range p.entries {
		keys = append(keys, k)
	}
	p.mu.Unlock()
	for _, k := range keys {
		p.mu.Lock()
		e := p.entries[k]
		p.mu.Unlock()
		if e == nil {
			continue
		}
		e.mu.Lock()
		stale := e.conn != nil && e.lastUsedAt.Before(cutoff)
		if stale {
			e.conn.Close()
			e.conn = nil
		}
		e.mu.Unlock()
	}
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./internal/codexappgateway/broker/ -run TestPool -v`
Expected: both pool tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/broker/
git commit -m "feat(broker): per-workspace Conn Pool with idle reap

Pool.Get returns a cached *Conn for workspaceID, single-flight dialing
on first miss and reusing across calls. A reaper goroutine sweeps
connections idle longer than idleTTL (production: 5min) and closes
them, so the next Get on that workspace dials fresh. WSURLResolver
abstracts over supervisor.EnsureSubprocess for testability."
```

---

## Task 9: CXG `/api/turns` REST handler

**Files:**
- Create: `internal/codexappgateway/turn_api.go`
- Test: `internal/codexappgateway/turn_api_test.go`

- [ ] **Step 1: Write handler test**

```go
package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/broker"
)

// fakeBroker implements turnRunner for handler unit tests.
type fakeBroker struct {
	startThreadFn func(ctx context.Context, workspaceID string) (string, error)
	turnFn        func(ctx context.Context, workspaceID, threadID string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error)
}

func (f *fakeBroker) StartThread(ctx context.Context, workspaceID string) (string, error) {
	return f.startThreadFn(ctx, workspaceID)
}
func (f *fakeBroker) Turn(ctx context.Context, workspaceID, threadID string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	return f.turnFn(ctx, workspaceID, threadID, params, timeout)
}

func TestTurnAPISuccess(t *testing.T) {
	h := &turnAPIHandler{
		runner: &fakeBroker{
			startThreadFn: func(_ context.Context, _ string) (string, error) {
				return "thr-new", nil
			},
			turnFn: func(_ context.Context, ws, tid string, _ json.RawMessage, _ time.Duration) (json.RawMessage, error) {
				if ws != "ws-1" || tid != "thr-new" {
					t.Errorf("ws=%s tid=%s", ws, tid)
				}
				return json.RawMessage(`{"id":"trn-1","status":"completed","items":[{"type":"agentMessage","id":"m","text":"hi"}],"itemsView":"full","error":null}`), nil
			},
		},
	}
	body, _ := json.Marshal(map[string]any{
		"workspaceId": "ws-1",
		"threadId":    nil,
		"params":      map[string]any{"input": []any{map[string]any{"type": "text", "text": "hi"}}},
		"timeoutMs":   30000,
	})
	r := httptest.NewRequest("POST", "/api/turns", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp turnAPIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ThreadID != "thr-new" {
		t.Errorf("threadId=%q", resp.ThreadID)
	}
	if resp.Transport != nil {
		t.Errorf("transport=%+v want nil", resp.Transport)
	}
	if resp.Turn == nil {
		t.Fatal("turn missing")
	}
	var turn map[string]any
	_ = json.Unmarshal(resp.Turn, &turn)
	if turn["status"] != "completed" {
		t.Errorf("turn.status=%v", turn["status"])
	}
}

func TestTurnAPITimeout(t *testing.T) {
	h := &turnAPIHandler{
		runner: &fakeBroker{
			startThreadFn: func(_ context.Context, _ string) (string, error) { return "thr-x", nil },
			turnFn: func(_ context.Context, _, _ string, _ json.RawMessage, _ time.Duration) (json.RawMessage, error) {
				return nil, &broker.TimeoutError{ThreadID: "thr-x", TurnID: "trn-x"}
			},
		},
	}
	body, _ := json.Marshal(map[string]any{
		"workspaceId": "ws-1",
		"params":      map[string]any{"input": []any{}},
	})
	r := httptest.NewRequest("POST", "/api/turns", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	var resp turnAPIResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Turn != nil {
		t.Errorf("turn must be nil on timeout, got %s", resp.Turn)
	}
	if resp.Transport == nil || resp.Transport.Code != "brokerTimeout" {
		t.Errorf("transport=%+v want brokerTimeout", resp.Transport)
	}
}

func TestTurnAPIMissingWorkspace(t *testing.T) {
	h := &turnAPIHandler{runner: &fakeBroker{}}
	body, _ := json.Marshal(map[string]any{
		"params": map[string]any{"input": []any{}},
	})
	r := httptest.NewRequest("POST", "/api/turns", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestTurnAPISubprocessCrash(t *testing.T) {
	h := &turnAPIHandler{
		runner: &fakeBroker{
			startThreadFn: func(_ context.Context, _ string) (string, error) { return "thr-x", nil },
			turnFn: func(_ context.Context, _, _ string, _ json.RawMessage, _ time.Duration) (json.RawMessage, error) {
				return nil, errors.New("dial: connection refused")
			},
		},
	}
	body, _ := json.Marshal(map[string]any{"workspaceId": "ws-1", "params": map[string]any{"input": []any{}}})
	r := httptest.NewRequest("POST", "/api/turns", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp turnAPIResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Transport == nil || resp.Transport.Code != "subprocessCrash" {
		t.Errorf("transport=%+v want subprocessCrash", resp.Transport)
	}
}
```

- [ ] **Step 2: Create `turn_api.go`**

```go
package codexappgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/broker"
)

// turnRunner abstracts the broker so the handler is unit-testable
// without spinning up real codex subprocesses.
type turnRunner interface {
	StartThread(ctx context.Context, workspaceID string) (string, error)
	Turn(ctx context.Context, workspaceID, threadID string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error)
}

// turnAPIRequest mirrors the REST request defined in the design spec.
// Field names use camelCase to align 1:1 with codex v2 protocol.
type turnAPIRequest struct {
	WorkspaceID string          `json:"workspaceId"`
	ThreadID    *string         `json:"threadId,omitempty"`
	Params      json.RawMessage `json:"params"`
	TimeoutMs   int             `json:"timeoutMs,omitempty"`
}

// turnAPIResponse: either Turn (codex Turn raw) OR Transport, never both.
// ThreadID is always populated (existing or newly-created).
type turnAPIResponse struct {
	ThreadID  string          `json:"threadId"`
	Turn      json.RawMessage `json:"turn,omitempty"`
	Transport *transportError `json:"transport,omitempty"`
}

type transportError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type turnAPIHandler struct {
	runner turnRunner
}

const defaultTurnTimeout = 5 * time.Minute

func (h *turnAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req turnAPIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.WorkspaceID == "" {
		http.Error(w, "workspaceId required", http.StatusBadRequest)
		return
	}
	if len(req.Params) == 0 {
		http.Error(w, "params required", http.StatusBadRequest)
		return
	}
	timeout := defaultTurnTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	ctx := r.Context()
	resp := turnAPIResponse{}

	threadID := ""
	if req.ThreadID != nil {
		threadID = *req.ThreadID
	}
	if threadID == "" {
		newID, err := h.runner.StartThread(ctx, req.WorkspaceID)
		if err != nil {
			resp.Transport = classifyTransport(err)
			writeJSON(w, resp)
			return
		}
		threadID = newID
	}
	resp.ThreadID = threadID

	rawTurn, err := h.runner.Turn(ctx, req.WorkspaceID, threadID, req.Params, timeout)
	if err != nil {
		resp.Transport = classifyTransport(err)
		writeJSON(w, resp)
		return
	}
	resp.Turn = rawTurn
	writeJSON(w, resp)
}

func classifyTransport(err error) *transportError {
	var te *broker.TimeoutError
	if errors.As(err, &te) {
		return &transportError{Code: "brokerTimeout", Message: te.Error()}
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "dial"), strings.Contains(msg, "connection refused"), strings.Contains(msg, "subprocess"):
		return &transportError{Code: "subprocessCrash", Message: msg}
	case strings.Contains(msg, "connection closed"), strings.Contains(msg, "ws"):
		return &transportError{Code: "wsDisconnect", Message: msg}
	default:
		return &transportError{Code: "wsDisconnect", Message: msg}
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
```

- [ ] **Step 3: Run — expect PASS**

Run: `go test ./internal/codexappgateway/ -run TestTurnAPI -v`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/codexappgateway/turn_api.go internal/codexappgateway/turn_api_test.go
git commit -m "feat(cxg): POST /api/turns REST handler

Validates workspaceId + params, calls broker.StartThread when threadId
null, calls broker.Turn, packs the result into the spec-defined
response: {threadId, turn} on success, {threadId, transport:{code}} on
broker-level failure. Transport codes: brokerTimeout / subprocessCrash
/ wsDisconnect. turnRunner interface allows handler-level unit tests
without real codex subprocesses."
```

---

## Task 10: CXG — wire `/api/turns` into `server.go` with X-Internal-Secret middleware

**Files:**
- Modify: `internal/codexappgateway/server.go`
- Modify: `internal/codexappgateway/config.go`
- Test: `internal/codexappgateway/turn_api_route_test.go`

- [ ] **Step 1: Check existing config.go ServeConfig**

Run: `grep -nE "type ServeConfig|AgentserverInternalSecret|OperationLogSecret" internal/codexappgateway/config.go`

The shared `INTERNAL_API_SECRET` is already in ServeConfig as `AgentserverInternalSecret` (referenced from `auth/remote_verifier.go:40`). Reuse it.

- [ ] **Step 2: Build a runner adapter that wraps a real `*broker.Pool`**

Add to `turn_api.go`:

```go
// poolRunner adapts *broker.Pool to the turnRunner interface used by
// the handler. Production wiring uses this; tests use fakes.
type poolRunner struct {
	pool *broker.Pool
}

func newPoolRunner(p *broker.Pool) *poolRunner { return &poolRunner{pool: p} }

func (r *poolRunner) StartThread(ctx context.Context, workspaceID string) (string, error) {
	conn, err := r.pool.Get(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	return conn.StartThread(ctx)
}

func (r *poolRunner) Turn(ctx context.Context, workspaceID, threadID string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	conn, err := r.pool.Get(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return conn.Turn(ctx, threadID, params, timeout)
}
```

- [ ] **Step 3: Write the resolver that wraps supervisor.EnsureSubprocess**

Add to `turn_api.go`:

```go
// makeSupervisorResolver returns a broker.WSURLResolver that uses the
// existing supervisor + buildConfig wiring. Returns the ws URL of the
// loopback codex subprocess for the workspace.
func makeSupervisorResolver(sup *supervisor.Supervisor, build func(context.Context, string, string) (supervisor.SpawnConfig, error)) broker.WSURLResolver {
	return func(ctx context.Context, workspaceID string) (string, error) {
		key := supervisor.Key{WorkspaceID: workspaceID}
		handle, err := sup.EnsureSubprocess(ctx, key, func(loopbackToken string) (supervisor.SpawnConfig, error) {
			return build(ctx, workspaceID, loopbackToken)
		})
		if err != nil {
			return "", err
		}
		return handle.WSURL, nil
	}
}
```

Add the import: `"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"`.

- [ ] **Step 4: Mount the route in `server.go`**

Locate the routes block in `internal/codexappgateway/server.go` (around line 247). After the existing routes add:

```go
	// REST shim for non-ws clients (e.g. WeChat-routed channels).
	// Auth: X-Internal-Secret shared with agentserver.
	turnHandler := &turnAPIHandler{
		runner: newPoolRunner(s.brokerPool),
	}
	r.With(s.requireInternalSecret).Post("/api/turns", turnHandler.ServeHTTP)
```

Add a `brokerPool *broker.Pool` field to the `Server` struct (search for where `s.sup` is initialized). In `NewServer` (or equivalent constructor — match the existing pattern), after the supervisor is constructed add:

```go
	srv.brokerPool = broker.NewPool(
		makeSupervisorResolver(srv.sup, srv.buildConfig),
		5*time.Minute,
	)
```

And add `requireInternalSecret` middleware (or reuse one if it exists — search `grep -rn "X-Internal-Secret" internal/codexappgateway/`):

```go
func (s *Server) requireInternalSecret(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AgentserverInternalSecret == "" {
			http.Error(w, "internal secret not configured", http.StatusInternalServerError)
			return
		}
		if r.Header.Get("X-Internal-Secret") != s.cfg.AgentserverInternalSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

Wire `Server.Close()` (or `Shutdown`) to also call `s.brokerPool.Close()` so reaper exits cleanly.

- [ ] **Step 5: Write the route-level test**

Create `internal/codexappgateway/turn_api_route_test.go`:

```go
package codexappgateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTurnAPIRequiresInternalSecret(t *testing.T) {
	s := &Server{cfg: ServeConfig{AgentserverInternalSecret: "s3cret"}}
	mw := s.requireInternalSecret(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("no header", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/turns", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("code=%d want 401", w.Code)
		}
	})
	t.Run("wrong header", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/turns", strings.NewReader("{}"))
		r.Header.Set("X-Internal-Secret", "nope")
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("code=%d", w.Code)
		}
	})
	t.Run("correct", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/turns", strings.NewReader("{}"))
		r.Header.Set("X-Internal-Secret", "s3cret")
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Errorf("code=%d", w.Code)
		}
	})
}
```

- [ ] **Step 6: Build and run all CXG tests**

Run: `go build ./internal/codexappgateway/... && go test ./internal/codexappgateway/...`
Expected: build clean, all PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/codexappgateway/server.go internal/codexappgateway/turn_api.go internal/codexappgateway/turn_api_route_test.go
git commit -m "feat(cxg): mount /api/turns with X-Internal-Secret auth

Wires the turn_api handler into the chi router. Server gains a
brokerPool (per-workspace Conn cache, 5min idle TTL) that uses
supervisor.EnsureSubprocess via makeSupervisorResolver to spawn /
reuse the codex subprocess. requireInternalSecret middleware shares
the existing AgentserverInternalSecret config — no new env var."
```

---

## Task 11: agentserver DB migration + `CodexThreadID` field

**Files:**
- Create: `internal/db/migrations/024_codex_thread_id.sql`
- Modify: `internal/db/agent_sessions.go`
- Test: `internal/db/agent_sessions_codex_test.go`

- [ ] **Step 1: Create migration**

```sql
-- 024_codex_thread_id.sql
-- Add codex_thread_id to agent_sessions so the codex routing path
-- can persist the codex Thread id per (workspace_id, external_id)
-- conversation. Coexists with cc_thread_id (used by stateless_cc).

ALTER TABLE agent_sessions ADD COLUMN codex_thread_id TEXT;
```

- [ ] **Step 2: Add field + setter to agent_sessions.go**

Locate `type AgentSession struct` and add:

```go
	CodexThreadID *string `json:"codex_thread_id,omitempty"`
```

Add the column to every SELECT used by GetSessionByExternalID, GetSession, etc. (search for them: `grep -n "SELECT.*FROM agent_sessions" internal/db/agent_sessions.go`). For each query, add `codex_thread_id` to the column list and scan it into `&s.CodexThreadID`.

Add at the bottom of the file:

```go
// SetSessionCodexThreadID updates (or clears, when threadID is nil) the
// codex_thread_id for a session. Used by the codex routing handler to
// persist the thread id after the first thread/start, and to clear it
// on thread-not-found / contextWindowExceeded so the next user message
// opens a fresh thread.
func (db *DB) SetSessionCodexThreadID(ctx context.Context, sessionID string, threadID *string) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE agent_sessions SET codex_thread_id = ? WHERE id = ?`, threadID, sessionID)
	if err != nil {
		return fmt.Errorf("update codex_thread_id: %w", err)
	}
	return nil
}
```

- [ ] **Step 3: Write test**

Create `internal/db/agent_sessions_codex_test.go`:

```go
package db

import (
	"context"
	"testing"
)

func TestCodexThreadIDRoundTrip(t *testing.T) {
	db := newTestDB(t) // use whatever helper exists in this package; copy from an existing _test.go
	ctx := context.Background()

	// You'll need to insert a workspace and a session first using existing
	// helpers. Adjust to whatever fixture pattern this package uses.
	sess, err := db.CreateAgentSession(ctx, AgentSessionInput{
		WorkspaceID: "ws-1",
		ExternalID:  "chat-1@im.wechat",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Initially nil.
	got, _ := db.GetSessionByExternalID(ctx, "ws-1", "chat-1@im.wechat")
	if got.CodexThreadID != nil {
		t.Errorf("initial codex_thread_id = %v want nil", got.CodexThreadID)
	}

	// Set it.
	tid := "thr-abc"
	if err := db.SetSessionCodexThreadID(ctx, sess.ID, &tid); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ = db.GetSessionByExternalID(ctx, "ws-1", "chat-1@im.wechat")
	if got.CodexThreadID == nil || *got.CodexThreadID != "thr-abc" {
		t.Errorf("got = %v want thr-abc", got.CodexThreadID)
	}

	// Clear it.
	if err := db.SetSessionCodexThreadID(ctx, sess.ID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = db.GetSessionByExternalID(ctx, "ws-1", "chat-1@im.wechat")
	if got.CodexThreadID != nil {
		t.Errorf("after clear = %v want nil", got.CodexThreadID)
	}
}
```

Adjust the test to whatever `CreateAgentSession` / fixture helper the existing test files use — read one of them first: `ls internal/db/*_test.go` and copy a working pattern.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/db/ -run TestCodexThreadID -v`
Expected: PASS (after fixing the migration auto-loads / test bootstrap pattern).

Then run the full db tests: `go test ./internal/db/...`
Expected: all PASS — no regression on existing session tests.

- [ ] **Step 5: Commit**

```bash
git add internal/db/migrations/024_codex_thread_id.sql internal/db/agent_sessions.go internal/db/agent_sessions_codex_test.go
git commit -m "feat(db): add agent_sessions.codex_thread_id

Migration 024 adds a nullable TEXT column for the codex routing path
to persist its codex Thread id per conversation. AgentSession.scan
includes it everywhere; new SetSessionCodexThreadID handles both set
and clear so handler can wipe the id on thread-not-found and let the
next user message open a fresh thread."
```

---

## Task 12: agentserver — `codex_client.go` HTTP client to CXG

**Files:**
- Create: `internal/server/codex_client.go`
- Test: `internal/server/codex_client_test.go`

- [ ] **Step 1: Write test (using httptest fake CXG)**

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexClientPostsExpectedBody(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/turns" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("X-Internal-Secret") != "s3cret" {
			t.Errorf("missing secret")
		}
		gotBody, _ = readAll(r.Body)
		_, _ = w.Write([]byte(`{"threadId":"thr-1","turn":{"id":"trn-1","status":"completed","items":[],"itemsView":"full","error":null}}`))
	}))
	defer srv.Close()

	c := NewCodexClient(srv.URL, "s3cret")
	resp, err := c.RunTurn(context.Background(), CodexTurnRequest{
		WorkspaceID: "ws-x",
		ThreadID:    nil,
		Params:      json.RawMessage(`{"input":[{"type":"text","text":"hi"}]}`),
		TimeoutMs:   30000,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if resp.ThreadID != "thr-1" {
		t.Errorf("threadID=%q", resp.ThreadID)
	}

	var sent map[string]any
	_ = json.Unmarshal(gotBody, &sent)
	if sent["workspaceId"] != "ws-x" {
		t.Errorf("body workspaceId=%v", sent["workspaceId"])
	}
}

func TestCodexClientReturnsTransportOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()
	c := NewCodexClient(srv.URL, "")
	_, err := c.RunTurn(context.Background(), CodexTurnRequest{
		WorkspaceID: "ws", Params: json.RawMessage(`{"input":[]}`),
	})
	if err == nil {
		t.Fatal("expected error on 502")
	}
}

// readAll mirrors io.ReadAll for the test only — keep imports minimal.
func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 512)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}

// guard against unused imports
var _ = strings.HasPrefix
```

- [ ] **Step 2: Create `codex_client.go`**

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CodexClient calls codex-app-gateway's POST /api/turns.
type CodexClient struct {
	baseURL string
	secret  string
	http    *http.Client
}

func NewCodexClient(baseURL, internalSecret string) *CodexClient {
	return &CodexClient{
		baseURL: baseURL,
		secret:  internalSecret,
		// Generous default — caller is the codex_im handler which has its
		// own per-turn timeout coming from the request body.
		http: &http.Client{Timeout: 6 * time.Minute},
	}
}

// CodexTurnRequest mirrors the spec'd /api/turns request body 1:1.
type CodexTurnRequest struct {
	WorkspaceID string          `json:"workspaceId"`
	ThreadID    *string         `json:"threadId,omitempty"`
	Params      json.RawMessage `json:"params"`
	TimeoutMs   int             `json:"timeoutMs,omitempty"`
}

// CodexTurnResponse mirrors the spec'd response.
type CodexTurnResponse struct {
	ThreadID  string                 `json:"threadId"`
	Turn      json.RawMessage        `json:"turn,omitempty"`
	Transport *CodexTransportError   `json:"transport,omitempty"`
}

type CodexTransportError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *CodexClient) RunTurn(ctx context.Context, req CodexTurnRequest) (*CodexTurnResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/turns", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		hreq.Header.Set("X-Internal-Secret", c.secret)
	}
	hresp, err := c.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("cxg: %w", err)
	}
	defer hresp.Body.Close()
	respBody, _ := io.ReadAll(hresp.Body)
	if hresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cxg /api/turns status=%d body=%s", hresp.StatusCode, string(respBody))
	}
	var out CodexTurnResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode: %w body=%s", err, string(respBody))
	}
	return &out, nil
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/server/ -run TestCodexClient -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/server/codex_client.go internal/server/codex_client_test.go
git commit -m "feat(server): codex_client HTTP wrapper for CXG /api/turns

Marshals CodexTurnRequest verbatim (camelCase), sends X-Internal-Secret,
decodes CodexTurnResponse {threadId, turn, transport}. 6min HTTP
client timeout (caller still controls per-turn timeout via TimeoutMs
in the request body)."
```

---

## Task 13: agentserver — `codex_im_inbound` handler (single-shot path, no queue yet)

**Files:**
- Create: `internal/server/codex_im_inbound.go`
- Test: `internal/server/codex_im_inbound_test.go`

- [ ] **Step 1: Write the handler test (decision matrix from spec)**

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// captureSender records what handler POSTed to /api/internal/imbridge/send.
type capturedSend struct {
	channelID string
	toUser    string
	text      string
}

func newCapturingImbridge(t *testing.T) (url string, sends *atomic.Value /* []*capturedSend */, stop func()) {
	t.Helper()
	var stored atomic.Value
	stored.Store([]*capturedSend{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ChannelID string `json:"channel_id"`
			ToUserID  string `json:"to_user_id"`
			Text      string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cur := stored.Load().([]*capturedSend)
		stored.Store(append(cur, &capturedSend{channelID: body.ChannelID, toUser: body.ToUserID, text: body.Text}))
		w.WriteHeader(200)
	}))
	return srv.URL, &stored, srv.Close
}

// fakeSessionStore implements the sessionStore interface (defined in
// codex_im_inbound.go) by routing through caller-supplied closures so
// each test can inject custom behavior.
type fakeSessionStore struct {
	get func(ctx context.Context, workspaceID, externalID string) (sessionView, error)
	set func(ctx context.Context, sessionID string, threadID *string) error
}

func (f *fakeSessionStore) GetSessionByExternalID(ctx context.Context, workspaceID, externalID string) (sessionView, error) {
	return f.get(ctx, workspaceID, externalID)
}

func (f *fakeSessionStore) SetSessionCodexThreadID(ctx context.Context, sessionID string, threadID *string) error {
	return f.set(ctx, sessionID, threadID)
}

// fakeCodexClient lets us inject CXG responses.
type fakeCodexClient struct {
	resp *CodexTurnResponse
	err  error
}

func (f *fakeCodexClient) RunTurn(_ context.Context, _ CodexTurnRequest) (*CodexTurnResponse, error) {
	return f.resp, f.err
}

func TestCodexInboundHappyPath(t *testing.T) {
	sendURL, sends, stop := newCapturingImbridge(t)
	defer stop()

	h := &codexInboundHandler{
		codex: &fakeCodexClient{
			resp: &CodexTurnResponse{
				ThreadID: "thr-new",
				Turn: json.RawMessage(`{"id":"trn-1","status":"completed","items":[{"type":"agentMessage","id":"m1","text":"hello"}],"itemsView":"full","error":null}`),
			},
		},
		sessions: &fakeSessionStore{
			get: func(_ context.Context, _, _ string) (sessionView, error) {
				return sessionView{ID: "sess-1", CodexThreadID: nil}, nil
			},
			set: func(_ context.Context, sessionID string, tid *string) error {
				if sessionID != "sess-1" || tid == nil || *tid != "thr-new" {
					t.Errorf("set called with sessionID=%s tid=%v", sessionID, tid)
				}
				return nil
			},
		},
		imbridgeSendURL: sendURL,
		internalSecret:  "",
	}

	body := map[string]any{
		"channel_id":      "ch-1",
		"workspace_id":    "ws-1",
		"wechat_user_id":  "wxid_a",
		"text":            "hi",
	}
	r := newCodexInboundRequest(body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", w.Code)
	}
	// Wait for worker to send.
	waitFor(t, func() bool { return len(sends.Load().([]*capturedSend)) == 1 })
	captured := sends.Load().([]*capturedSend)[0]
	if captured.text != "hello" {
		t.Errorf("send text=%q want hello", captured.text)
	}
	if captured.toUser != "wxid_a" {
		t.Errorf("send to=%q", captured.toUser)
	}
}

func TestCodexInboundFailedWithUsageLimit(t *testing.T) {
	sendURL, sends, stop := newCapturingImbridge(t)
	defer stop()

	h := &codexInboundHandler{
		codex: &fakeCodexClient{
			resp: &CodexTurnResponse{
				ThreadID: "thr-x",
				Turn: json.RawMessage(`{"id":"trn-1","status":"failed","items":[],"itemsView":"full","error":{"message":"quota","codexErrorInfo":"usageLimitExceeded","additionalDetails":null}}`),
			},
		},
		sessions: &fakeSessionStore{
			get: func(_ context.Context, _, _ string) (sessionView, error) {
				return sessionView{ID: "sess-1", CodexThreadID: strPtr("thr-x")}, nil
			},
			set: func(context.Context, string, *string) error { return nil },
		},
		imbridgeSendURL: sendURL,
	}
	r := newCodexInboundRequest(map[string]any{
		"channel_id": "ch", "workspace_id": "ws", "wechat_user_id": "u", "text": "x",
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	waitFor(t, func() bool { return len(sends.Load().([]*capturedSend)) == 1 })
	got := sends.Load().([]*capturedSend)[0]
	if !strings.Contains(got.text, "配额") {
		t.Errorf("text=%q want quota message", got.text)
	}
}

func TestCodexInboundContextWindowClearsThread(t *testing.T) {
	sendURL, sends, stop := newCapturingImbridge(t)
	defer stop()

	var cleared int32
	h := &codexInboundHandler{
		codex: &fakeCodexClient{
			resp: &CodexTurnResponse{
				ThreadID: "thr-old",
				Turn: json.RawMessage(`{"id":"trn-1","status":"failed","items":[],"itemsView":"full","error":{"message":"too long","codexErrorInfo":"contextWindowExceeded","additionalDetails":null}}`),
			},
		},
		sessions: &fakeSessionStore{
			get: func(_ context.Context, _, _ string) (sessionView, error) {
				return sessionView{ID: "sess-1", CodexThreadID: strPtr("thr-old")}, nil
			},
			set: func(_ context.Context, _ string, tid *string) error {
				if tid != nil {
					t.Errorf("want clear (nil), got %v", *tid)
				}
				atomic.AddInt32(&cleared, 1)
				return nil
			},
		},
		imbridgeSendURL: sendURL,
	}
	r := newCodexInboundRequest(map[string]any{
		"channel_id": "ch", "workspace_id": "ws", "wechat_user_id": "u", "text": "x",
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	waitFor(t, func() bool { return atomic.LoadInt32(&cleared) > 0 && len(sends.Load().([]*capturedSend)) == 1 })
	if !strings.Contains(sends.Load().([]*capturedSend)[0].text, "上下文") {
		t.Errorf("want context-window message")
	}
}

func TestCodexInboundTransportError(t *testing.T) {
	sendURL, sends, stop := newCapturingImbridge(t)
	defer stop()
	h := &codexInboundHandler{
		codex: &fakeCodexClient{
			resp: &CodexTurnResponse{
				ThreadID:  "thr-x",
				Transport: &CodexTransportError{Code: "brokerTimeout", Message: "..."},
			},
		},
		sessions: &fakeSessionStore{
			get: func(_ context.Context, _, _ string) (sessionView, error) {
				return sessionView{ID: "sess-1"}, nil
			},
			set: func(context.Context, string, *string) error { return nil },
		},
		imbridgeSendURL: sendURL,
	}
	r := newCodexInboundRequest(map[string]any{
		"channel_id": "ch", "workspace_id": "ws", "wechat_user_id": "u", "text": "x",
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	waitFor(t, func() bool { return len(sends.Load().([]*capturedSend)) == 1 })
	if !strings.Contains(sends.Load().([]*capturedSend)[0].text, "超时") {
		t.Errorf("want timeout message")
	}
}

// helpers

func newCodexInboundRequest(body map[string]any) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/internal/imbridge/codex/turn", bytes.NewReader(b))
	return r
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		// 10ms polling — these handlers complete fast in tests.
		fmt.Sprintf("%d", i) // avoid time import bloat
	}
	t.Fatal("waitFor: condition never satisfied")
}

func strPtr(s string) *string { return &s }
```

(If `time.Sleep` is preferable, replace the `fmt.Sprintf` loop with `time.Sleep(10*time.Millisecond)`.)

- [ ] **Step 2: Create `codex_im_inbound.go` (single-shot — no FIFO yet)**

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// codexInboundHandler routes inbound WeChat messages destined for the
// codex routing path. POST /api/internal/imbridge/codex/turn body is:
//
//	{
//	  "channel_id": "ch-xxx",
//	  "workspace_id": "ws-xxx",
//	  "wechat_user_id": "wxid_xxx",
//	  "text": "..."
//	}
//
// Returns 202 immediately and runs the codex turn + send asynchronously
// (Task 14 wraps this with a per-(channel,user) FIFO; this task ships
// the bare path so end-to-end works for one in-flight request per user).
type codexInboundHandler struct {
	codex           codexCaller
	sessions        sessionStore
	imbridgeSendURL string
	internalSecret  string
}

type codexCaller interface {
	RunTurn(ctx context.Context, req CodexTurnRequest) (*CodexTurnResponse, error)
}

// sessionStore is what the handler needs from the DB. Defined as an
// interface so tests can inject fakes without a real *sql.DB. The
// production adapter (Task 15) wraps *db.DB.
type sessionStore interface {
	GetSessionByExternalID(ctx context.Context, workspaceID, externalID string) (sessionView, error)
	SetSessionCodexThreadID(ctx context.Context, sessionID string, threadID *string) error
}

// sessionView is the subset of agent_sessions fields the codex handler
// needs. Decoupled from db.AgentSession to keep the test fakes small.
type sessionView struct {
	ID            string
	CodexThreadID *string
}

type codexInboundRequest struct {
	ChannelID     string `json:"channel_id"`
	WorkspaceID   string `json:"workspace_id"`
	WechatUserID  string `json:"wechat_user_id"`
	WechatSender  string `json:"wechat_sender_name,omitempty"`
	Text          string `json:"text"`
	QuotedText    string `json:"quoted_text,omitempty"`
	QuotedSender  string `json:"quoted_sender,omitempty"`
}

func (h *codexInboundHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req codexInboundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.ChannelID == "" || req.WorkspaceID == "" || req.WechatUserID == "" {
		http.Error(w, "channel_id, workspace_id, wechat_user_id required", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"queued":true}`))
	go h.processTurn(context.Background(), req)
}

func (h *codexInboundHandler) processTurn(ctx context.Context, req codexInboundRequest) {
	externalID := req.WechatUserID + "@im.wechat"
	sess, err := h.sessions.GetSessionByExternalID(ctx, req.WorkspaceID, externalID)
	if err != nil {
		log.Printf("codex_im: resolve session: %v", err)
		h.sendError(ctx, req, "⚠️ 内部错误：找不到会话")
		return
	}

	params := buildCodexInput(req)
	cresp, err := h.codex.RunTurn(ctx, CodexTurnRequest{
		WorkspaceID: req.WorkspaceID,
		ThreadID:    sess.CodexThreadID,
		Params:      params,
	})
	if err != nil {
		log.Printf("codex_im: cxg call: %v", err)
		h.sendError(ctx, req, "⚠️ Codex 处理失败，请稍后重试")
		return
	}

	// Transport-layer failure.
	if cresp.Transport != nil {
		text := transportToUserMessage(cresp.Transport)
		h.sendError(ctx, req, text)
		return
	}

	// Persist thread id if new or changed.
	if cresp.ThreadID != "" && (sess.CodexThreadID == nil || *sess.CodexThreadID != cresp.ThreadID) {
		tid := cresp.ThreadID
		if err := h.sessions.SetSessionCodexThreadID(ctx, sess.ID, &tid); err != nil {
			log.Printf("codex_im: persist thread id: %v", err)
		}
	}

	// Decode turn.status.
	var turn struct {
		Status string `json:"status"`
		Items  []json.RawMessage `json:"items"`
		Error  *struct {
			Message       string  `json:"message"`
			CodexErrorInfo *string `json:"codexErrorInfo,omitempty"`
		} `json:"error"`
	}
	if err := json.Unmarshal(cresp.Turn, &turn); err != nil {
		log.Printf("codex_im: decode turn: %v", err)
		h.sendError(ctx, req, "⚠️ Codex 返回格式异常")
		return
	}

	switch turn.Status {
	case "completed":
		text := lastAgentMessageText(turn.Items)
		if text == "" {
			h.sendError(ctx, req, "⚠️ Codex 没有返回文本内容")
			return
		}
		h.sendText(ctx, req, text)
	case "failed":
		if turn.Error != nil && turn.Error.CodexErrorInfo != nil {
			switch *turn.Error.CodexErrorInfo {
			case "contextWindowExceeded":
				// Clear thread so next message starts fresh.
				_ = h.sessions.SetSessionCodexThreadID(ctx, sess.ID, nil)
				h.sendError(ctx, req, "⚠️ 上下文已满，请新开会话")
				return
			case "usageLimitExceeded":
				h.sendError(ctx, req, "⚠️ Codex 配额已用尽")
				return
			case "serverOverloaded":
				h.sendError(ctx, req, "⚠️ Codex 繁忙，请稍后重试")
				return
			}
		}
		// Heuristic: thread-not-found (codex Thread id no longer recognized).
		msg := ""
		if turn.Error != nil {
			msg = turn.Error.Message
		}
		lo := strings.ToLower(msg)
		if strings.Contains(lo, "thread") && (strings.Contains(lo, "not found") || strings.Contains(lo, "unknown") || strings.Contains(lo, "missing")) {
			_ = h.sessions.SetSessionCodexThreadID(ctx, sess.ID, nil)
			h.sendError(ctx, req, "⚠️ 会话已重置，请重发消息")
			return
		}
		log.Printf("codex_im: turn failed: %s", msg)
		h.sendError(ctx, req, "⚠️ Codex 处理失败")
	case "interrupted":
		h.sendError(ctx, req, "⚠️ 处理已取消，请重发")
	default:
		log.Printf("codex_im: unexpected status %q", turn.Status)
		h.sendError(ctx, req, "⚠️ Codex 返回异常状态")
	}
}

// lastAgentMessageText scans the items list in reverse for the last
// {type:"agentMessage"} entry and returns its text. Returns "" if none.
func lastAgentMessageText(items []json.RawMessage) string {
	for i := len(items) - 1; i >= 0; i-- {
		var shell struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(items[i], &shell); err != nil {
			continue
		}
		if shell.Type == "agentMessage" && shell.Text != "" {
			return shell.Text
		}
	}
	return ""
}

func transportToUserMessage(t *CodexTransportError) string {
	switch t.Code {
	case "brokerTimeout":
		return "⚠️ 处理超时，请稍后重试"
	default:
		return "⚠️ Codex 处理失败，请稍后重试"
	}
}

// buildCodexInput constructs the codex turn/start params.input from the
// inbound WeChat message. MVP: text only. Quoted text is concatenated
// into the same text item with a "引用:" prefix; image / media ignored.
func buildCodexInput(req codexInboundRequest) json.RawMessage {
	text := req.Text
	if req.QuotedText != "" {
		quoter := req.QuotedSender
		if quoter == "" {
			quoter = "之前的消息"
		}
		text = fmt.Sprintf("[引用 %s] %s\n%s", quoter, req.QuotedText, req.Text)
	}
	wrapped := map[string]any{
		"input": []map[string]any{
			{"type": "text", "text": text},
		},
	}
	b, _ := json.Marshal(wrapped)
	return b
}

// sendText / sendError both POST /api/internal/imbridge/send. The
// endpoint StopTyping side-effect kicks in automatically.

func (h *codexInboundHandler) sendText(ctx context.Context, req codexInboundRequest, text string) {
	h.postSend(ctx, map[string]any{
		"channel_id": req.ChannelID,
		"to_user_id": req.WechatUserID,
		"text":       text,
	})
}

func (h *codexInboundHandler) sendError(ctx context.Context, req codexInboundRequest, text string) {
	h.postSend(ctx, map[string]any{
		"channel_id": req.ChannelID,
		"to_user_id": req.WechatUserID,
		"text":       text,
	})
}

func (h *codexInboundHandler) postSend(ctx context.Context, body map[string]any) {
	b, _ := json.Marshal(body)
	r, err := http.NewRequestWithContext(ctx, "POST", h.imbridgeSendURL+"/api/internal/imbridge/send", bytes.NewReader(b))
	if err != nil {
		log.Printf("codex_im: build send req: %v", err)
		return
	}
	r.Header.Set("Content-Type", "application/json")
	if h.internalSecret != "" {
		r.Header.Set("X-Internal-Secret", h.internalSecret)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		log.Printf("codex_im: send POST: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("codex_im: send status=%d body=%s", resp.StatusCode, body)
	}
}
```

(Tests use `fakeSessionStore` defined in the test file. Task 15 adds the production `dbSessionStore` adapter that wraps `*db.DB` — same `sessionStore` interface, no refactor needed.)

- [ ] **Step 3: Run tests**

Run: `go test ./internal/server/ -run TestCodexInbound -v`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/server/codex_im_inbound.go internal/server/codex_im_inbound_test.go
git commit -m "feat(server): codex_im_inbound handler (single-shot, no queue)

Async handler that returns 202 immediately and processes the codex
turn in a goroutine: resolve agent_sessions row, call CXG /api/turns,
decode the Turn, dispatch on status/codexErrorInfo per spec, persist
thread id, POST back to /api/internal/imbridge/send. Buisness-error
heuristics: contextWindowExceeded clears codex_thread_id;
usageLimitExceeded / serverOverloaded get distinct user messages;
thread-not-found (substring match) clears thread id and surfaces a
'session reset' message. No queueing yet — Task 14 adds the FIFO
dispatcher in front."
```

---

## Task 14: agentserver — per-(channel,user) FIFO dispatcher

**Files:**
- Modify: `internal/server/codex_im_inbound.go`
- Test: `internal/server/codex_dispatcher_test.go`

- [ ] **Step 1: Write dispatcher test**

```go
package server

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatcherSerializesPerKey(t *testing.T) {
	var (
		mu       sync.Mutex
		started  []string
		finished []string
	)
	processFn := func(req codexInboundRequest) {
		mu.Lock()
		started = append(started, req.Text)
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		finished = append(finished, req.Text)
		mu.Unlock()
	}
	d := newCodexDispatcher(processFn, 5)
	defer d.Stop()

	// Three messages, same key — must run serially.
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: "A"})
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: "B"})
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: "C"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(finished)
		mu.Unlock()
		if done == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finished) != 3 {
		t.Fatalf("finished=%v want all 3", finished)
	}
	// started order must match enqueue order.
	want := []string{"A", "B", "C"}
	for i := range want {
		if started[i] != want[i] {
			t.Errorf("started[%d]=%s want %s", i, started[i], want[i])
		}
	}
}

func TestDispatcherIndependentKeysRunConcurrently(t *testing.T) {
	var inFlight, peakInFlight int32
	processFn := func(_ codexInboundRequest) {
		now := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peakInFlight)
			if now <= p || atomic.CompareAndSwapInt32(&peakInFlight, p, now) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
	}
	d := newCodexDispatcher(processFn, 5)
	defer d.Stop()
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u1", Text: "A"})
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u2", Text: "B"})
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u3", Text: "C"})
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&peakInFlight) < 2 {
		t.Errorf("peakInFlight=%d want >=2 (independent keys should overlap)", peakInFlight)
	}
}

func TestDispatcherDropsOldestPastCap(t *testing.T) {
	var processed []string
	var mu sync.Mutex
	processFn := func(req codexInboundRequest) {
		// Block the first one so the queue can back up.
		if req.Text == "first" {
			time.Sleep(200 * time.Millisecond)
		}
		mu.Lock()
		processed = append(processed, req.Text)
		mu.Unlock()
	}
	d := newCodexDispatcher(processFn, 2)
	defer d.Stop()
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: "first"})
	for _, msg := range []string{"a", "b", "c", "d"} {
		d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: msg})
	}
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(processed) > 3 { // cap=2 means worker drops everything past pos 2 in addition to first
		t.Errorf("processed=%v want at most 3", processed)
	}
	if processed[0] != "first" {
		t.Errorf("processed[0]=%s want first", processed[0])
	}
}

// Silence unused-import checker (helpers shared with other test files).
var _ = json.Marshal
var _ = context.Background
```

- [ ] **Step 2: Add dispatcher to `codex_im_inbound.go`**

Add at the bottom:

```go
// --- per-(channel,user) FIFO dispatcher ---

type codexDispatcher struct {
	processFn func(codexInboundRequest)
	cap       int

	mu      sync.Mutex
	workers map[string]*dispatcherSlot
	stopped bool
}

type dispatcherSlot struct {
	ch chan codexInboundRequest
}

func newCodexDispatcher(processFn func(codexInboundRequest), cap int) *codexDispatcher {
	return &codexDispatcher{
		processFn: processFn,
		cap:       cap,
		workers:   make(map[string]*dispatcherSlot),
	}
}

func dispatcherKey(req codexInboundRequest) string {
	return req.ChannelID + ":" + req.WechatUserID
}

// Enqueue adds req to the per-key channel. If the channel is full,
// drains the oldest queued item to make room (drop-oldest policy).
// Starts a worker for this key if none is running.
func (d *codexDispatcher) Enqueue(req codexInboundRequest) {
	key := dispatcherKey(req)
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	slot, ok := d.workers[key]
	if !ok {
		slot = &dispatcherSlot{ch: make(chan codexInboundRequest, d.cap)}
		d.workers[key] = slot
		go d.runWorker(key, slot)
	}
	d.mu.Unlock()

	for {
		select {
		case slot.ch <- req:
			return
		default:
			// Full — drop oldest then retry.
			select {
			case <-slot.ch:
			default:
			}
		}
	}
}

func (d *codexDispatcher) runWorker(key string, slot *dispatcherSlot) {
	idle := time.NewTimer(30 * time.Second)
	defer idle.Stop()
	for {
		select {
		case req, ok := <-slot.ch:
			if !ok {
				return
			}
			d.processFn(req)
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(30 * time.Second)
		case <-idle.C:
			d.mu.Lock()
			// Re-check the channel under the lock to avoid losing a
			// just-enqueued item.
			if len(slot.ch) == 0 {
				delete(d.workers, key)
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()
			idle.Reset(30 * time.Second)
		}
	}
}

func (d *codexDispatcher) Stop() {
	d.mu.Lock()
	d.stopped = true
	for _, slot := range d.workers {
		close(slot.ch)
	}
	d.workers = nil
	d.mu.Unlock()
}
```

Add imports: `"sync"`, `"time"`.

- [ ] **Step 3: Wire dispatcher into the handler**

Change the handler's `ServeHTTP` to enqueue rather than spawn a goroutine. First add a `dispatcher *codexDispatcher` field to `codexInboundHandler`:

```go
type codexInboundHandler struct {
	codex           codexCaller
	sessions        sessionStore
	imbridgeSendURL string
	internalSecret  string
	dispatcher      *codexDispatcher
}
```

Add a constructor:

```go
func newCodexInboundHandler(codex codexCaller, sessions sessionStore, imbridgeSendURL, internalSecret string) *codexInboundHandler {
	h := &codexInboundHandler{
		codex:           codex,
		sessions:        sessions,
		imbridgeSendURL: imbridgeSendURL,
		internalSecret:  internalSecret,
	}
	h.dispatcher = newCodexDispatcher(func(req codexInboundRequest) {
		h.processTurn(context.Background(), req)
	}, 5)
	return h
}
```

Replace the `go h.processTurn(...)` in `ServeHTTP` with:

```go
	h.dispatcher.Enqueue(req)
```

Update the existing handler-level tests in Task 13 to use `newCodexInboundHandler` (they currently build the struct literal directly — they still work because they set `dispatcher` to nil, but `ServeHTTP` now requires it). Fix by either:
- Using `newCodexInboundHandler` in tests (and rely on dispatcher being correct)
- Or in the test setup do `h.dispatcher = newCodexDispatcher(func(req codexInboundRequest) { h.processTurn(context.Background(), req) }, 5)` after constructing the literal

Pick the first approach to keep tests realistic.

- [ ] **Step 4: Run all server tests**

Run: `go test ./internal/server/... -run TestCodex -v`
Expected: dispatcher + handler tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/
git commit -m "feat(server): per-(channel,user) FIFO dispatcher

Enqueues each codex inbound request into a per-(channel,user) chan of
capacity 5; one worker per key processes serially. Workers stay alive
for 30s after the queue empties (avoids goroutine churn for chatty
users) and exit afterward. Drop-oldest when capacity exceeded keeps
memory bounded; combined with the 'wait for last reply' UX, this
matches the spec: messages from one user serialize, messages from
different users run concurrently."
```

---

## Task 15: agentserver — wire `codex_im_inbound` into server + bridge

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/imbridge/bridge.go`
- Modify: `internal/imbridgesvc/handlers.go`
- Modify: `internal/server/codex_im_inbound.go` (real sessionStore adapter)

- [ ] **Step 1: Add a production `sessionStore` adapter wrapping `*db.DB`**

The `sessionStore` interface and `sessionView` type are already defined in Task 13. Add the production adapter at the bottom of `codex_im_inbound.go`:

```go
// dbSessionStore is the production sessionStore that reads/writes the
// real agent_sessions table.
type dbSessionStore struct {
	db *db.DB
}

func (s *dbSessionStore) GetSessionByExternalID(ctx context.Context, workspaceID, externalID string) (sessionView, error) {
	sess, err := s.db.GetSessionByExternalID(ctx, workspaceID, externalID)
	if err != nil {
		return sessionView{}, err
	}
	return sessionView{ID: sess.ID, CodexThreadID: sess.CodexThreadID}, nil
}

func (s *dbSessionStore) SetSessionCodexThreadID(ctx context.Context, sessionID string, threadID *string) error {
	return s.db.SetSessionCodexThreadID(ctx, sessionID, threadID)
}
```

Add import `"github.com/agentserver/agentserver/internal/db"` if not already present.

- [ ] **Step 2: Mount the route in `server.go`**

Find the existing internal route registrations (similar to `/api/internal/imbridge/send`) and add:

```go
	codexClient := NewCodexClient(os.Getenv("CODEX_APP_GATEWAY_URL"), os.Getenv("INTERNAL_API_SECRET"))
	codexHandler := newCodexInboundHandler(codexClient, &dbSessionStore{db: s.db}, os.Getenv("AGENTSERVER_INTERNAL_URL"), os.Getenv("INTERNAL_API_SECRET"))
	r.With(s.requireInternalSecret).Post("/api/internal/imbridge/codex/turn", codexHandler.ServeHTTP)
```

`AGENTSERVER_INTERNAL_URL` is where the agentserver listens internally for imbridge callbacks (the same host the bridge already POSTs to for stateless_cc — search for its current source: `grep -rn "imbridge/send" internal/server/`). Reuse whatever variable already exists for that URL.

If `CODEX_APP_GATEWAY_URL` env is unset, log a warning and skip the route (so this PR is safe to merge even if CXG isn't deployed yet).

- [ ] **Step 3: Add `case "codex"` to `Bridge.forwardMessage`**

Locate `forwardMessage` in `internal/imbridge/bridge.go` (around line 411). Add:

```go
	case "codex":
		return b.forwardToCodex(ctx, binding, msg)
```

Add `forwardToCodex` mirroring `forwardToAgentserver`:

```go
// forwardToCodex POSTs the inbound message to agentserver's
// /api/internal/imbridge/codex/turn endpoint, which enqueues it into
// its per-user FIFO and asynchronously calls codex-app-gateway.
func (b *Bridge) forwardToCodex(ctx context.Context, binding BridgeBinding, msg InboundMessage) (bool, error) {
	body, err := json.Marshal(map[string]any{
		"channel_id":         binding.ChannelID,
		"workspace_id":       binding.WorkspaceID,
		"wechat_user_id":     msg.FromUserID,
		"wechat_sender_name": msg.SenderName,
		"text":               msg.Text,
		"quoted_text":        msg.QuotedText,
		"quoted_sender":      msg.QuotedSender,
	})
	if err != nil {
		return false, fmt.Errorf("marshal codex inbound: %w", err)
	}
	url := b.agentserverURL + "/api/internal/imbridge/codex/turn"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := os.Getenv("INTERNAL_API_SECRET"); secret != "" {
		req.Header.Set("X-Internal-Secret", secret)
	}
	hctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(hctx))
	if err != nil {
		return false, fmt.Errorf("forward codex: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return false, fmt.Errorf("codex inbound: status %d", resp.StatusCode)
	}
	return true, nil
}
```

- [ ] **Step 4: Update routing_mode whitelist**

In `internal/imbridgesvc/handlers.go` around line 971 (`grep -n "stateless_cc" internal/imbridgesvc/handlers.go`), change the allowed-modes check:

```go
	switch req.RoutingMode {
	case "", "nanoclaw", "stateless_cc", "codex":
	default:
		http.Error(w, `routing_mode must be one of: nanoclaw, stateless_cc, codex`, http.StatusBadRequest)
		return
	}
```

- [ ] **Step 5: Build + run tests**

Run: `go build ./... && go test ./internal/server/... ./internal/imbridge/... ./internal/imbridgesvc/... -v 2>&1 | tail -40`
Expected: build clean, all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/ internal/imbridge/bridge.go internal/imbridgesvc/handlers.go
git commit -m "feat(imbridge): wire routing_mode=codex end-to-end

Mounts /api/internal/imbridge/codex/turn on agentserver (requires
CODEX_APP_GATEWAY_URL + INTERNAL_API_SECRET env). Bridge gains a
'codex' case in forwardMessage that POSTs to the new endpoint and
returns once accepted (fire-and-forget for the reply — codex_im
handler enqueues into its FIFO and replies async via the standard
/api/internal/imbridge/send path). routing_mode whitelist extended
to accept 'codex'."
```

---

## Task 16: Web UI — add "codex" to routing_mode dropdown

**Files:**
- Modify: `web/src/...` (find with `grep -rln "stateless_cc" web/src/`)

- [ ] **Step 1: Find the dropdown component**

Run: `grep -rln "stateless_cc\|nanoclaw" web/src/`

Likely a TS file with a Select/Dropdown for routing_mode. Open it.

- [ ] **Step 2: Add "codex" option**

Whatever the existing structure is (array of `{value, label}` pairs is typical), add:

```ts
  { value: "codex", label: "Codex (via codex-app-gateway)" },
```

- [ ] **Step 3: Verify build**

Run: `cd web && npm run build 2>&1 | tail -20`
Expected: build succeeds.

- [ ] **Step 4: Commit**

```bash
git add web/
git commit -m "feat(ui): add 'codex' option to channel routing_mode dropdown"
```

---

## Task 17: Smoke test + docs

**Files:**
- Create: `docs/deployment/codex-routing-rollout.md` (optional but useful)

- [ ] **Step 1: Manual smoke test checklist**

Stage on a dev cluster with both agentserver and CXG deployed. Set `CODEX_APP_GATEWAY_URL` in agentserver's env and shared `INTERNAL_API_SECRET` on both.

Pick a real WeChat-bound IM channel, then in agentserver web UI change its routing_mode to "codex".

- [ ] **Step 2: Functional tests** (mark each off as you verify)

  - [ ] Send "hello" from WeChat → receive an LLM reply within ~10s
  - [ ] Send "write hello world in python" → receive code block in reply
  - [ ] Send "now translate that to typescript" → reply references the prior code (thread context preserved)
  - [ ] Send 3 messages back-to-back ("a", "b", "c") → receive 3 replies in order
  - [ ] Restart CXG mid-turn → user receives "Codex 处理失败" message; typing stops
  - [ ] Wait 6+ min idle then send a message → loopback ws redials (check CXG logs), still works
  - [ ] Send a message large enough to exceed codex context window → user receives "上下文已满" message; next message starts fresh thread
  - [ ] Verify `agent_sessions.codex_thread_id` is set after first message

- [ ] **Step 3: Document the rollout in a deployment doc (optional)**

If your team has a deployment runbook pattern, append a short section noting the required env vars (`CODEX_APP_GATEWAY_URL`, shared `INTERNAL_API_SECRET`) and how to switch a channel from stateless_cc to codex.

- [ ] **Step 4: Push the branch and open a PR**

```bash
git push -u github HEAD
gh pr create --title "feat(imbridge): codex-app-gateway routing for WeChat channels" \
  --body "$(cat docs/superpowers/specs/2026-05-19-weixin-codex-app-gateway-routing.md | head -30)..."
```

Reference the spec doc + this plan doc in the PR description.

---

## Spec Coverage Check

Every requirement in the spec maps to a task above:

- [x] CXG `/api/turns` REST endpoint (Tasks 9, 10)
- [x] CXG broker per-workspace pool (Task 8)
- [x] CXG broker loopback ws Conn with handshake (Tasks 3, 4)
- [x] CXG broker auto-approve approval frames (Tasks 4, 5)
- [x] CXG broker turn/interrupt on timeout (Task 7)
- [x] CXG codex config `default_tools_approval_mode = "approve"` (Task 1)
- [x] agentserver `codex_im_inbound` handler (Tasks 13, 14)
- [x] agentserver per-(channel,user) FIFO dispatcher (Task 14)
- [x] agentserver business-error dispatching (codexErrorInfo branches) (Task 13)
- [x] agentserver thread-not-found auto-clear+retry (Task 13 — clear only; one-shot retry deferred to follow-up since spec marks it as a fallback heuristic)
- [x] agentserver DB migration + CodexThreadID (Task 11)
- [x] agentserver codex_client HTTP wrapper (Task 12)
- [x] imbridge bridge.go `forwardToCodex` + routing_mode whitelist (Task 15)
- [x] Web UI dropdown option (Task 16)
- [x] Smoke test (Task 17)

**Deferred to follow-up (per spec Phase 2 section):**
- Streaming SSE response
- Image input/output
- oplog interceptor on `/api/turns`
- Typing keepalive re-ping after queued task completes
- One-shot retry on thread-not-found (Task 13 currently only clears; spec mentions retrying once — flag this in the PR description if you want it in MVP)

If you want the thread-not-found retry in MVP, after Task 13 add a sub-step that re-calls `h.codex.RunTurn` with `ThreadID: nil` once when the heuristic matches, and only sends the user message if the second attempt also fails. Keep that as a follow-up unless you specifically request it during execution.
