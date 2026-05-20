# codex-app-gateway: ApprovalFilter + protocolVersion cleanup (PR 2) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close Gaps 4 + part of Gap 5 from [`2026-05-20-codex-gateway-full-alignment-design.md`](../specs/2026-05-20-codex-gateway-full-alignment-design.md). Today the auto-accept approval logic only fires on the REST `/turn` path through `broker`; the transparent `/codex-app/ws` path forwards approval requests to the caller, which most SDK clients don't know how to answer. This PR moves the approval logic into a shared package and wires it into the transparent path. Same PR also deletes the bogus `protocolVersion: "2025-06-18"` field that the broker sends to upstream (upstream's `InitializeParams` has no such field).

**Architecture:** Extract `broker/protocol.go::approvalReply` + `isApprovalRequest` + method constants into a new `internal/codexappgateway/approvalfilter/` package. The package exports `TryReply(frame []byte) ([]byte, bool)` — given an incoming server→client JSON-RPC frame, returns either the full synthesized response (jsonrpc 2.0 envelope with the same id) and `true`, or `nil, false` for non-approval frames. Wire into `handleCodexAppWS` via `wsbridge.Interceptor.OnServerFrame`. Broker keeps its existing call site but delegates to the shared package.

**Tech Stack:** Go, `nhooyr.io/websocket`, `internal/wsbridge` (existing Interceptor/RunProxyWithInterceptor primitives), stdlib `encoding/json`.

---

## File structure

**Create:**
- `internal/codexappgateway/approvalfilter/filter.go` — shared approval-reply logic + `TryReply` synthesis
- `internal/codexappgateway/approvalfilter/filter_test.go` — TDD tests covering all 5 methods + non-approval + malformed frame
- `internal/codexappgateway/approval_intercept_test.go` — integration test against `handleCodexAppWS` with a fake upstream

**Modify:**
- `internal/codexappgateway/server.go` (`handleCodexAppWS`) — replace `wsbridge.RunProxy(...)` with `wsbridge.RunProxyWithInterceptor(...)`, wiring `approvalfilter.TryReply` into `OnServerFrame`
- `internal/codexappgateway/broker/protocol.go` — delete `methodItem*` constants, `isApprovalRequest`, `approvalReply` (now in shared package); keep imports clean
- `internal/codexappgateway/broker/conn.go:128` — call `approvalfilter.Reply(method)` instead of local `approvalReply`
- `internal/codexappgateway/broker/conn.go:57` — remove `"protocolVersion":"2025-06-18"` from initialize payload (upstream `InitializeParams` doesn't have that field; serde silently drops it; cleanup for clarity)
- `internal/codexappgateway/broker/conn_approval_test.go` — adjust import or expectations if the test poked at the now-private internals

---

## Task 1: `approvalfilter` package — Reply + TryReply (TDD)

**Files:**
- Create: `internal/codexappgateway/approvalfilter/filter.go`
- Create: `internal/codexappgateway/approvalfilter/filter_test.go`

### Test interface to design against

The package exports:
- `func Methods() []string` — exhaustive list of approval methods (used by codex-pin CI lint, see PR 1's codex-pin.json `approval_methods`)
- `func Reply(method string) json.RawMessage` — for a known approval method, returns the `result` payload that codex expects. Returns `{}` for unknown approval methods (defensive).
- `func TryReply(frame []byte) (response []byte, isApproval bool)` — high-level helper for the wsbridge interceptor. Parses the JSON-RPC frame; if it's a server→client approval request (has `method` matching an approval method AND has `id`), returns the complete `{jsonrpc, id, result}` response bytes ready to write back to upstream. Returns `nil, false` otherwise.

### Steps

- [ ] **Step 1: Write failing tests**

Create `internal/codexappgateway/approvalfilter/filter_test.go`:
```go
package approvalfilter

import (
	"encoding/json"
	"testing"
)

func TestMethods_ListsAllFive(t *testing.T) {
	got := Methods()
	want := []string{
		"item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
		"item/tool/requestUserInput",
		"mcpServer/elicitation/request",
	}
	if len(got) != len(want) {
		t.Fatalf("Methods() len: got %d, want %d", len(got), len(want))
	}
	got1 := map[string]bool{}
	for _, m := range got {
		got1[m] = true
	}
	for _, m := range want {
		if !got1[m] {
			t.Errorf("Methods() missing %q", m)
		}
	}
}

func TestReply_KnownMethods(t *testing.T) {
	cases := []struct {
		method string
		want   string
	}{
		{"item/commandExecution/requestApproval", `{"decision":"accept"}`},
		{"item/fileChange/requestApproval", `{"decision":"accept"}`},
		{"item/permissions/requestApproval", `{"permissions":{}}`},
		{"item/tool/requestUserInput", `{"answers":{}}`},
		{"mcpServer/elicitation/request", `{"action":"accept","content":null,"_meta":null}`},
	}
	for _, tc := range cases {
		got := Reply(tc.method)
		if string(got) != tc.want {
			t.Errorf("Reply(%q): got %s, want %s", tc.method, got, tc.want)
		}
	}
}

func TestReply_UnknownMethod(t *testing.T) {
	got := Reply("item/somethingNew/requestApproval")
	if string(got) != `{}` {
		t.Errorf("Reply(unknown): got %s, want {}", got)
	}
}

func TestTryReply_ApprovalRequest(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":42,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	resp, ok := TryReply(frame)
	if !ok {
		t.Fatal("TryReply: got isApproval=false, want true")
	}
	var got struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("response not valid JSON: %v\nresp=%s", err, resp)
	}
	if got.JSONRPC != "2.0" {
		t.Errorf("jsonrpc: got %q, want 2.0", got.JSONRPC)
	}
	if got.ID != 42 {
		t.Errorf("id: got %d, want 42", got.ID)
	}
	if string(got.Result) != `{"decision":"accept"}` {
		t.Errorf("result: got %s, want {\"decision\":\"accept\"}", got.Result)
	}
}

func TestTryReply_NonApprovalFrame(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{}}`)
	resp, ok := TryReply(frame)
	if ok {
		t.Errorf("TryReply on notification: got isApproval=true, want false (resp=%s)", resp)
	}
}

func TestTryReply_ApprovalWithoutID(t *testing.T) {
	// Approval request without id is malformed — treat as non-approval (can't reply).
	frame := []byte(`{"jsonrpc":"2.0","method":"item/commandExecution/requestApproval","params":{}}`)
	_, ok := TryReply(frame)
	if ok {
		t.Errorf("TryReply on approval-without-id: got isApproval=true, want false")
	}
}

func TestTryReply_MalformedJSON(t *testing.T) {
	frame := []byte(`{not valid json`)
	_, ok := TryReply(frame)
	if ok {
		t.Errorf("TryReply on malformed JSON: got isApproval=true, want false")
	}
}

func TestTryReply_PreservesStringID(t *testing.T) {
	// JSON-RPC ids can be strings, numbers, or null. Codex uses numbers in practice,
	// but the spec allows strings. The synthesized response must echo the id back
	// verbatim regardless of type.
	frame := []byte(`{"jsonrpc":"2.0","id":"abc-123","method":"item/fileChange/requestApproval","params":{}}`)
	resp, ok := TryReply(frame)
	if !ok {
		t.Fatal("TryReply: got isApproval=false, want true")
	}
	if !contains(resp, []byte(`"id":"abc-123"`)) {
		t.Errorf("response should preserve string id: %s", resp)
	}
}

func contains(haystack, needle []byte) bool {
	return len(haystack) >= len(needle) && byteContains(haystack, needle)
}

func byteContains(s, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
```

(`contains` is a local utility because `bytes.Contains` would require an extra import that the test file otherwise doesn't need. Importing `"bytes"` and using `bytes.Contains` is also fine — engineer's choice.)

- [ ] **Step 2: Run test, confirm failure**

```bash
cd /root/agentserver
go test ./internal/codexappgateway/approvalfilter -v
```
Expected: compile error — package doesn't exist.

- [ ] **Step 3: Implement `filter.go`**

Create `internal/codexappgateway/approvalfilter/filter.go`:
```go
// Package approvalfilter synthesizes auto-accept responses for codex
// app-server's server-to-client approval/elicitation requests.
//
// Codex pushes 5 kinds of approval-style requests at the gateway during
// a turn (item/commandExecution/requestApproval, etc.). Without a
// response, the turn stalls. Codex is configured with
// default_tools_approval_mode = "approve" so these requests rarely
// fire — but when they do, this package returns the schema-valid
// auto-accept payload.
//
// Payloads track upstream codex schemas at the tag pinned in
// codex-pin.json. The pin's CI lint scans upstream for new approval
// methods on each bump; if a new method appears, add it here.
package approvalfilter

import (
	"encoding/json"
)

const (
	methodItemCmdApproval   = "item/commandExecution/requestApproval"
	methodItemFileApproval  = "item/fileChange/requestApproval"
	methodItemPermsApproval = "item/permissions/requestApproval"
	methodItemUserInput     = "item/tool/requestUserInput"
	methodMcpElicitation    = "mcpServer/elicitation/request"
)

// Methods returns the exhaustive list of approval method names.
// Used by codex-pin CI lint and by integration tests.
func Methods() []string {
	return []string{
		methodItemCmdApproval,
		methodItemFileApproval,
		methodItemPermsApproval,
		methodItemUserInput,
		methodMcpElicitation,
	}
}

// IsApproval reports whether method is an approval-style server-to-client
// request that needs a synthesized reply.
func IsApproval(method string) bool {
	switch method {
	case methodItemCmdApproval, methodItemFileApproval, methodItemPermsApproval,
		methodItemUserInput, methodMcpElicitation:
		return true
	}
	return false
}

// Reply returns the JSON-RPC `result` payload for a known approval method.
// For commandExecution/fileChange we use {"decision":"accept"} (most
// permissive variant of the enum). For permissions we send
// {"permissions":{}} (no extra grants). For requestUserInput we send no
// answers. For mcpServer/elicitation we send action:"accept" with null
// content. For unknown methods, returns "{}" as a defensive default.
//
// Payload shapes match codex v2 enum/struct definitions in
// app-server-protocol/src/protocol/v2/ at the pinned tag.
func Reply(method string) json.RawMessage {
	switch method {
	case methodItemCmdApproval, methodItemFileApproval:
		return json.RawMessage(`{"decision":"accept"}`)
	case methodItemPermsApproval:
		return json.RawMessage(`{"permissions":{}}`)
	case methodItemUserInput:
		return json.RawMessage(`{"answers":{}}`)
	case methodMcpElicitation:
		return json.RawMessage(`{"action":"accept","content":null,"_meta":null}`)
	}
	return json.RawMessage(`{}`)
}

// TryReply inspects a server-to-client JSON-RPC frame. If it's an
// approval request (recognised method + present id), returns the
// complete {jsonrpc, id, result} response bytes ready to write back to
// upstream, along with true. Otherwise returns nil, false.
//
// Never blocks; never errors. Malformed frames return nil, false.
func TryReply(frame []byte) ([]byte, bool) {
	var f struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		return nil, false
	}
	if !IsApproval(f.Method) {
		return nil, false
	}
	if len(f.ID) == 0 || string(f.ID) == "null" {
		// Server-sent notification (no id) — can't reply.
		return nil, false
	}
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      f.ID,
		Result:  Reply(f.Method),
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, false
	}
	return b, true
}
```

- [ ] **Step 4: Run tests, all pass**

```bash
go test ./internal/codexappgateway/approvalfilter -v -count=1
```
Expected: all 7 tests PASS.

- [ ] **Step 5: go vet**

```bash
go vet ./internal/codexappgateway/approvalfilter
```
Expected: clean.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/codexappgateway/approvalfilter/
git commit -m "feat(codex-app-gateway): approvalfilter package with TryReply helper"
```

---

## Task 2: Wire `approvalfilter` into `handleCodexAppWS` (transparent ws path)

**Files:**
- Modify: `internal/codexappgateway/server.go` (`handleCodexAppWS`)
- Create: `internal/codexappgateway/approval_intercept_test.go`

### Steps

- [ ] **Step 1: Write failing test**

Create `internal/codexappgateway/approval_intercept_test.go`. This test stands up the gateway with a stub supervisor that returns a fake app-server ws URL, then connects as a client, has the fake app-server push an approval request, and asserts the gateway responds within 100ms and the client never sees the request.

Plan a minimal integration test using the patterns from `integration_test.go` and `server_test.go`:

```go
package codexappgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// TestHandleCodexAppWS_ApprovalIntercept verifies the transparent ws
// path auto-replies to server-pushed approval requests without forwarding
// them to the caller.
func TestHandleCodexAppWS_ApprovalIntercept(t *testing.T) {
	// fakeAppServer: a tiny ws server playing the role of `codex app-server`.
	// On any client connection, it sends an approval request and reads back
	// the response.
	approvalReceived := make(chan json.RawMessage, 1)
	fakeAppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Logf("fake app-server accept: %v", err)
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")

		// Push an approval request immediately.
		req := []byte(`{"jsonrpc":"2.0","id":1,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := ws.Write(ctx, websocket.MessageText, req); err != nil {
			t.Logf("fake app-server write: %v", err)
			return
		}

		// Expect a response back from the gateway's filter.
		_, body, err := ws.Read(ctx)
		if err != nil {
			t.Logf("fake app-server read: %v", err)
			return
		}
		approvalReceived <- body
	}))
	defer fakeAppServer.Close()

	// Construct the gateway server with a stub supervisor pointing at
	// fakeAppServer. Reuse the test-helper machinery in server_test.go.
	srv := newTestServerWithFakeChild(t, fakeAppServer.URL)
	defer srv.Close()

	// Connect a client to the gateway's /codex-app/ws endpoint.
	wsURL := "ws" + srv.URL[4:] + "/codex-app/ws" // http→ws
	clientWS, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testToken(t, srv)}},
	})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer clientWS.Close(websocket.StatusNormalClosure, "done")

	// Wait for the gateway-synthesized response to land on the fake app-server.
	select {
	case body := <-approvalReceived:
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("response not JSON: %v\nbody=%s", err, body)
		}
		if resp.ID != 1 {
			t.Errorf("response id: got %d, want 1", resp.ID)
		}
		if string(resp.Result) != `{"decision":"accept"}` {
			t.Errorf("response result: got %s, want auto-accept", resp.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not synthesize approval response within 2s")
	}

	// The client side MUST NOT see the approval request frame (filter drops it).
	clientCtx, clientCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer clientCancel()
	_, payload, rerr := clientWS.Read(clientCtx)
	if rerr == nil {
		t.Errorf("client unexpectedly received a frame: %s", payload)
	}
	// Timeout on Read is the expected outcome.
}
```

The helpers `newTestServerWithFakeChild`, `testToken` may need to be added or adapted. Look at `server_test.go` and `server_testhelper_test.go` for existing helpers — extend, don't duplicate.

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver
go test ./internal/codexappgateway -run TestHandleCodexAppWS_ApprovalIntercept -v -timeout 15s
```
Expected: FAIL — the gateway forwards the frame to the client, doesn't synthesize a response.

- [ ] **Step 3: Wire the interceptor**

In `internal/codexappgateway/server.go` `handleCodexAppWS` (around line 341), replace:

```go
if err := wsbridge.RunProxy(ctx, userWS, childWS, func() { s.sup.Touch(key) }); err != nil {
    s.logger.Info("proxy ended", "err", err, "key", key)
}
```

with:

```go
intc := wsbridge.Interceptor{
    OnServerFrame: func(frame []byte) []byte {
        if resp, ok := approvalfilter.TryReply(frame); ok {
            // Synthesized response goes back upstream; the request frame
            // is dropped so the caller never sees it. Codex expects the
            // response on the same ws (server-to-client request).
            if werr := childWS.Write(ctx, websocket.MessageText, resp); werr != nil {
                s.logger.Warn("approval-filter: write reply",
                    "err", werr, "key", key)
            }
            return wsbridge.DropFrame
        }
        return nil
    },
}

if err := wsbridge.RunProxyWithInterceptor(ctx, userWS, childWS, intc, func() { s.sup.Touch(key) }); err != nil {
    s.logger.Info("proxy ended", "err", err, "key", key)
}
```

Add import:
```go
"github.com/agentserver/agentserver/internal/codexappgateway/approvalfilter"
```

- [ ] **Step 4: Run test, confirm pass**

```bash
go test ./internal/codexappgateway -run TestHandleCodexAppWS_ApprovalIntercept -v -timeout 15s -count=3
```
Run 3× to catch any timing flakiness. Expected: all PASS.

- [ ] **Step 5: Run full package suite**

```bash
go test ./internal/codexappgateway -count=1
```
Expected: all PASS, no regressions.

- [ ] **Step 6: Race detector**

```bash
go test ./internal/codexappgateway -race -count=1 -timeout 180s
```
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/codexappgateway/
git commit -m "feat(codex-app-gateway): wire approvalfilter into transparent ws path"
```

---

## Task 3: Broker cleanup — delegate to shared package + remove bogus protocolVersion

**Files:**
- Modify: `internal/codexappgateway/broker/protocol.go` — delete `methodItem*` constants, `isApprovalRequest`, `approvalReply`
- Modify: `internal/codexappgateway/broker/conn.go` — call `approvalfilter.Reply` / `approvalfilter.IsApproval` instead; remove bogus `protocolVersion` field

### Steps

- [ ] **Step 1: Read broker/conn.go current state**

```bash
grep -n 'isApprovalRequest\|approvalReply\|protocolVersion' /root/agentserver/internal/codexappgateway/broker/conn.go
```

You should see (approximately):
- Line ~57: `initialize` payload contains `"protocolVersion":"2025-06-18"`
- Line ~128: `approvalReply(f.Method)` call inside the read loop's notification dispatch

- [ ] **Step 2: Update broker/conn.go**

Edit line 57's initialize payload to remove `protocolVersion`:

Current:
```go
if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "initialize", Params: json.RawMessage(`{"clientInfo":{"name":"agentserver-codex-broker","version":"0.1.0"},"protocolVersion":"2025-06-18","capabilities":{}}`)}); err != nil {
```

Change to:
```go
if err := c.writeJSON(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "initialize", Params: json.RawMessage(`{"clientInfo":{"name":"agentserver-codex-broker","version":"0.1.0"},"capabilities":{}}`)}); err != nil {
```

Edit line ~128 (and its surrounding `isApproval` check, however that's spelled) to use the shared package:

Find the block that looks like:
```go
if isApprovalRequest(f.Method) {
    // ... build response with approvalReply(f.Method) ...
}
```

Change to use `approvalfilter.IsApproval` and `approvalfilter.Reply`:
```go
if approvalfilter.IsApproval(f.Method) {
    // ... build response with approvalfilter.Reply(f.Method) ...
}
```

Add import to `broker/conn.go`:
```go
"github.com/agentserver/agentserver/internal/codexappgateway/approvalfilter"
```

- [ ] **Step 3: Delete the orphaned local helpers from broker/protocol.go**

Open `internal/codexappgateway/broker/protocol.go` and DELETE the entire block:
- `methodItemCmdApproval`, `methodItemFileApproval`, `methodItemUserInput`, `methodItemPermsApproval`, `methodMcpElicitation` constants
- `isApprovalRequest` function
- `approvalReply` function

The header comment block above this section explaining "Approval frames we auto-reply to" can be deleted with it.

- [ ] **Step 4: Run broker tests**

```bash
cd /root/agentserver
go test ./internal/codexappgateway/broker -count=1 -v
```
Expected: all tests PASS (no behavior change — only internal restructuring).

If any test fails because it referenced the now-deleted private names (`isApprovalRequest`, `approvalReply`, `methodItem*`), update them to use the public `approvalfilter` API. The test fix should be minimal: replace the private identifier with the public one.

- [ ] **Step 5: Run full package suite**

```bash
go test ./internal/codexappgateway/... -count=1
```
Expected: all PASS.

- [ ] **Step 6: go vet**

```bash
go vet ./internal/codexappgateway/...
```
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/codexappgateway/broker/
git commit -m "refactor(codex-app-gateway): broker delegates to approvalfilter, drop bogus protocolVersion field"
```

---

## Task 4: Final verification + PR

- [ ] **Step 1: Full vet + test**

```bash
cd /root/agentserver
go vet ./...
go test ./... -count=1 -timeout 5m
```
Both clean / all PASS.

- [ ] **Step 2: codex-pin check (should be unaffected but verify)**

```bash
make codex-pin-check 2>&1 | tail -3
```
Expected: `codex-pin: OK`. (Note: this PR adds a new package whose method list MUST match `codex-pin.json`'s `approval_methods`. If they ever diverge, the PR is wrong. The test `TestMethods_ListsAllFive` in Task 1 enforces this on agentserver's side; codex-pin's CI lint enforces the upstream side.)

- [ ] **Step 3: Push + create PR**

```bash
git push -u github codex-app-gw-approval-filter
gh pr create --title "feat: ApprovalFilter on transparent ws + drop bogus protocolVersion (PR 2/3)" --body "$(cat <<'EOF'
## Summary

PR 2 of 3 from spec `2026-05-20-codex-gateway-full-alignment-design.md`. Closes Gap 4 (ApprovalFilter missing on transparent ws path) and the surgical half of Gap 5 (delete bogus `protocolVersion: "2025-06-18"` field from broker's initialize payload).

### What lands

- New package `internal/codexappgateway/approvalfilter/` with `Methods()`, `IsApproval(method)`, `Reply(method)`, and `TryReply(frame)` helpers
- `handleCodexAppWS` (the transparent `/codex-app/ws` and `/` endpoints) now intercepts the 5 approval-style server→client requests and auto-replies, matching the REST `/turn` path's existing behavior
- Broker's local `approvalReply` deleted in favor of the shared package (no behavior change for REST callers)
- Broker no longer sends `"protocolVersion":"2025-06-18"` — that field is not in upstream's `InitializeParams`; it was silently dropped by serde

## Out of scope (deferred to PR 3)

- REST `/turn` pivot to shared proxy core (the broker's separate JSON-RPC plumbing stays as-is)

## Test plan

- [x] 7 tests in `approvalfilter` package (5 methods, non-approval, malformed JSON, string-id preservation)
- [x] Integration test: fake app-server pushes approval request; gateway responds in <2s, client never sees the frame
- [x] All existing broker tests pass with the delegation
- [x] `go vet ./...` clean
- [x] `go test ./... -race` clean
- [ ] CI green

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Self-review notes

Spec coverage:
- Gap 4 (ApprovalFilter on transparent ws) → Tasks 1 + 2
- Gap 5 surgical half (delete bogus protocolVersion) → Task 3 step 2

Type consistency: `approvalfilter.Reply` returns `json.RawMessage` matching the broker's existing `approvalReply` return type. `approvalfilter.TryReply` returns `([]byte, bool)` — convenient for `wsbridge.Interceptor.OnServerFrame` which receives `[]byte` and returns `[]byte`.

No placeholders.
