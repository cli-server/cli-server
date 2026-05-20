# codex-app-gateway + codex-exec-gateway — Full Upstream Alignment

**Status:** PR 1 + PR 2 in flight; **PR 3 deferred** — see § "PR 3 deferral note" near the bottom of this file
**Date:** 2026-05-20
**Owner:** agentserver / codex integration
**Pin target:** upstream `openai/codex` tag **`rust-v0.131.0-alpha.22`** (latest stable as of 2026-05-20)

**Scope correction note (2026-05-20):** initial audit understated how much of the alignment work was already in place. The transparent JSON-RPC ws reverse proxy, the `wsbridge.Interceptor` filter abstraction, the supervisor lifecycle, and the bounded 64 MiB read limit on the app-gw side are **already implemented**. This spec only covers the concrete remaining gaps.

**Sibling specs:**
- [`2026-05-17-codex-exec-gateway-bridge-multiplexing.md`](2026-05-17-codex-exec-gateway-bridge-multiplexing.md) — relay frame multiplexing; this spec extends with operational limits

## Goal

Close five concrete gaps that prevent codex-app-gateway and codex-exec-gateway from being **provably aligned** with a pinned upstream codex version, and prevent silent drift from re-introducing the misalignments after they're fixed.

## What's already done (no work needed)

These were missing from the original audit but exist today:

| Component | Where |
|---|---|
| Transparent JSON-RPC ws reverse proxy | `internal/codexappgateway/server.go:264` `/codex-app/ws` and `/` route to `handleCodexAppWS` |
| Bidirectional frame pump | `internal/wsbridge/wsbridge.go::RunProxy` and `RunProxyWithInterceptor` |
| Filter abstraction (forward / rewrite / drop / short-circuit) | `wsbridge.Interceptor` with `DropFrame` sentinel — exactly the design pattern needed |
| Subprocess supervisor + GC | `supervisor.EnsureSubprocess` + `Touch`-based idle reaper |
| Bearer auth + workspace binding | `auth.ExtractBearer` + `auth.Verify` |
| 64 MiB read limit on caller ws | `userWS.SetReadLimit(maxWSFrameBytes)` at server.go:319 |
| Short-circuit response pattern | `oplog.TryHandleOperationsList` demonstrates `userWS.Write(...) + return DropFrame` |
| Approval reply schemas | `broker/protocol.go::approvalReply` (4 methods + default fallback) with embedded comments referencing upstream `v2/` enum variants |

The transparent proxy is **already** the way new SDK clients talk to the gateway. Upstream codex methods added in `rust-v0.131.0` (e.g., `thread/fork`, `thread/goal/*`, `turn/steer`) **already work** through `/codex-app/ws` without any gateway code change — the audit's "5 methods only" finding was only true for the REST `/turn` path going through `broker`.

## Five gaps

### Gap 1 — codex-pin machinery (versioned alignment proof)

No mechanism today guarantees the gateway's protocol assumptions match a specific upstream version. `internal/relaypb/relay.proto` is byte-identical to upstream `codex.exec_server.relay.v1.proto` at commit `ac466c0dbd` purely by manual copy; nothing fails the build if upstream diverges or our copy rots. Same for `approvalReply`'s embedded payload shapes — they reference v2 enum variants in comments only.

**Fix:**
- `codex-pin.json` at repo root pins upstream tag + sha + sha256 of tracked artifacts:
  - `codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto`
  - `codex-rs/app-server-protocol/src/protocol/v1.rs` (InitializeParams schema)
  - `codex-rs/app-server-protocol/src/protocol/v2/{item,mcp,notification}.rs` (approval/elicitation method names + response shapes)
- `cmd/check-codex-pin/main.go` (Go program; invoked from CI and from `make codex-pin-check`):
  1. Read `codex-pin.json`.
  2. `git ls-remote https://github.com/openai/codex.git refs/tags/<tag>` — assert sha matches.
  3. For each tracked file, fetch the blob at the pinned sha (via GitHub API or shallow clone), compute sha256, fail on mismatch.
  4. Assert `internal/relaypb/relay.proto` matches the upstream relay proto byte-for-byte.
  5. Grep upstream `v2/` for `requestApproval` / `elicitation/request` method strings; fail if any not listed in `broker/protocol.go::approvalReply`'s switch.
  6. **Soft check**: if upstream `main` has moved past the pinned sha for any tracked file, emit a CI warning with one-line diff. Non-fatal.
- `make codex-pin-bump TAG=<new-tag>`:
  - Re-runs the verification against the new tag.
  - Updates `codex-pin.json` with new shas.
  - Overwrites `internal/relaypb/relay.proto` from upstream, regenerates `relay.pb.go`.
  - Warns if new approval methods exist that aren't in `approvalReply` — engineer adds them and commits.

### Gap 2 — exec-gateway unbounded reads

`inbound.go:54` and `bridge.go:99` both call `ws.SetReadLimit(-1)`. The comment on inbound says it's because "codex exec-server streams large process/read responses" but no upper bound exists — a buggy or hostile executor can write multi-GiB frames and exhaust gateway memory.

**Fix:**
- Replace both with `ws.SetReadLimit(cfg.MaxFrameBytes)`. Default 16 MiB — well above any legitimate `process/read` chunk (codex-rs uses ~64 KiB-ish chunks by default) but bounded.
- Make it configurable via `CODEX_EXEC_GATEWAY_MAX_FRAME_BYTES` env var for emergency override.
- On read-limit violation, the existing `nhooyr/websocket` library closes with code 1009 (Message Too Big), which the bridge client's relay layer will surface as a stream error.

### Gap 3 — exec-gateway per-bridge idle timeout & stream cap

A bridge session (client `BridgeClient` ↔ gateway) can wedge silently if neither side sends frames. Today nothing reaps it; the inbound executor connection holds a route entry forever. With multiplexing landed (2026-05-17 spec), unbounded orphaned routes hurt: each one is a UUID slot, a goroutine, and a per-route mutex on the inbound writer.

**Fix:**
- Per-bridge idle timeout. Default 5 min, configurable. On idle expiry, gateway sends `RelayMessageFrame{body: Reset{reason:"idle-timeout"}}` on both the bridge ws and the inbound ws (so the executor's per-stream JSON-RPC session terminates cleanly), then closes the bridge ws. Inbound conn unaffected.
- Per-executor concurrent-bridge cap. Default 32 (the 2026-05-17 spec didn't bound this; in practice each is one MCP child × one tool concurrency = needs to scale with sub-agent count, 32 is generous). Beyond cap, `/bridge/{exe_id}` returns 503.

### Gap 4 — ApprovalFilter missing on transparent ws path

Today only the REST `/turn` path (through `broker`) intercepts and auto-accepts approval requests. The transparent `/codex-app/ws` path forwards them to the caller. As noted in `broker/protocol.go::approvalReply`'s comment, this rarely fires in practice (codex is configured with `default_tools_approval_mode = "approve"`), but it's a real divergence between paths and a foot-gun if a caller doesn't implement the protocol.

**Fix:**
- Move `approvalReply` from `broker/protocol.go` to a new `internal/codexappgateway/approvalfilter/` package.
- In `handleCodexAppWS`, add an `OnServerFrame` callback to the `wsbridge.Interceptor` that:
  - Parses the frame as JSON-RPC.
  - If `method` matches an approval method and `id` is present (server-to-client request), call `approvalReply(method)`, write the synthesized response back through `childWS.Write(...)`, return `DropFrame` to swallow the frame so the caller never sees it.
  - Otherwise return `nil` (forward unchanged).
- Tests: spin up a fake `app-server` that pushes each of the 5 approval methods; assert gateway responds in <100ms and caller sees no frame.

### Gap 5 — broker still sends bogus `protocolVersion` + 25+ methods unreachable via REST

`broker/conn.go:55` includes `"protocolVersion":"2025-06-18"` in `initialize`. Upstream `InitializeParams` has no such field — serde silently drops it. Harmless but wrong, and confusing future readers.

Separately, the broker hardcodes only 3 RPCs (`thread/start`, `turn/start`, `turn/interrupt`), so REST `/turn` callers cannot reach `thread/resume`, `thread/fork`, `thread/list`, `turn/steer`, etc. The REST `/turn` API surface is intentionally narrow ("run one turn, return the result"), so most of these have no REST analogue and don't need exposure. But the broker-as-a-library is also indirectly used (e.g., `oplog/operations_list_test.go` shows oplog hooking the broker's flow); a future internal caller might need other methods.

**Fix:**
- **Surgical**: remove `"protocolVersion":"2025-06-18"` field from the `initialize` payload at `broker/conn.go:55`. No behavior change.
- **Consolidation (deduplicate with transparent path)**: refactor `broker.Conn` to internally use the same `wsbridge.RunProxyWithInterceptor` + supervisor pattern that `handleCodexAppWS` uses, with a small in-process JSON-RPC client driving the REST→ws translation. Concretely:
  - Extract `proxyOptions` struct from `handleCodexAppWS` (filter chain + supervisor key + caller ws).
  - REST `/turn` handler builds an in-process pipe-pair (or a goroutine-local JSON-RPC channel) that plays the role of `userWS`, drives `initialize` → optional `thread/start` → `turn/start` → wait for `turn/completed`, then closes.
  - Delete `broker/conn.go`'s bespoke read loop, ID tracking, and approval handling (now in shared filter).
  - Net change: `broker/` shrinks by ~300 LOC; `internal/codexappgateway/restadapter/` gains ~150 LOC; `approvalfilter/` gains ~80 LOC.
- All existing tests in `turn_api_test.go`, `integration_test.go`, `broker/conn_*_test.go` either pass unchanged (REST adapter tests) or migrate to test the shared proxy core (former broker tests). Approval tests migrate to `approvalfilter/`.

## Migration plan

Three PRs. Order is: pin machinery + exec-gw hardening together (low risk, no app-gw refactor), then app-gw filter port, then REST consolidation.

### PR 1 — codex-pin + exec-gateway hardening
- Add `codex-pin.json`, `scripts/check-codex-pin.sh`, CI wiring.
- exec-gw: `SetReadLimit(MaxFrameBytes)`, `BridgeIdleTimeout`, `MaxStreamsPerExecutor`.
- Tests: pin verification (golden + drift); exec-gw limit enforcement.
- ~600 LOC scripts/Go + ~400 test LOC. **~2 days.**

### PR 2 — ApprovalFilter on transparent ws + delete bogus protocolVersion field
- New `internal/codexappgateway/approvalfilter/` package (move + tidy current `approvalReply`).
- `handleCodexAppWS` adds `OnServerFrame` callback wiring the filter.
- Delete `"protocolVersion":"2025-06-18"` field from `broker/conn.go:55`.
- Tests: per-method approval interception against fake app-server.
- ~150 LOC + ~250 test LOC. **~half day.**

### PR 3 — REST /turn pivots to shared proxy core
- Extract proxy plumbing from `handleCodexAppWS` into a reusable `proxy.Run(opts)` function.
- REST `/turn` rewrites internally to use the same primitives.
- Delete duplicated logic in `broker/conn.go`.
- All existing tests must pass without modification (this is a behavior-preserving refactor).
- Net ~−300 LOC + minor test migrations. **~1-1.5 days.**

**Total: ~3.5-4 days, 3 reviewable PRs.**

## Wire protocol details

### Approval interceptor (Gap 4) — handler shape

```go
// internal/codexappgateway/approvalfilter/filter.go
package approvalfilter

// Reply returns ({jsonrpc, id, result}) for an approval-style server-to-client
// request. Caller is responsible for writing the bytes back upstream.
// Returns nil if method is not an approval method.
func Reply(frame []byte) (response []byte, isApproval bool) { ... }

// Methods returns the exhaustive set of approval method names. Used by
// codex-pin's CI check to detect upstream additions.
func Methods() []string {
    return []string{
        "item/commandExecution/requestApproval",
        "item/fileChange/requestApproval",
        "item/permissions/requestApproval",
        "item/tool/requestUserInput",
        "mcpServer/elicitation/request",
    }
}
```

Wired into `handleCodexAppWS`:

```go
intc := wsbridge.Interceptor{
    OnServerFrame: func(frame []byte) []byte {
        if resp, ok := approvalfilter.Reply(frame); ok {
            // synthesized response goes back to upstream subprocess
            _ = childWS.Write(ctx, websocket.MessageText, resp)
            return wsbridge.DropFrame    // caller never sees the request
        }
        // ... existing oplog logic
        return nil
    },
}
```

### codex-pin.json initial state

```json
{
  "upstream_repo": "openai/codex",
  "tag": "rust-v0.131.0-alpha.22",
  "sha": "<resolved at PR-1 authoring time>",
  "tracked_files": {
    "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto": "<sha256 from upstream>",
    "codex-rs/app-server-protocol/src/protocol/v1.rs": "<sha256>",
    "codex-rs/app-server-protocol/src/protocol/v2/item.rs": "<sha256>",
    "codex-rs/app-server-protocol/src/protocol/v2/mcp.rs": "<sha256>"
  },
  "approval_methods": [
    "item/commandExecution/requestApproval",
    "item/fileChange/requestApproval",
    "item/permissions/requestApproval",
    "item/tool/requestUserInput",
    "mcpServer/elicitation/request"
  ]
}
```

The initial CI run after PR 1 lands should pass without any extra change, because `internal/relaypb/relay.proto` is already byte-identical to upstream `ac466c0dbd`, which is the relay.proto introduction and has not changed since.

## Testing

- **Pin check golden**: `codex-pin.json` matches upstream at the pinned sha. Corrupt the JSON, assert CI fails with a specific error pointing to the mismatched field.
- **Pin check drift**: fake `git ls-remote` returning a newer upstream sha; assert soft warning emitted, hard exit code 0.
- **exec-gw read limit**: write a 17 MiB frame from a fake executor; assert close code 1009 and no panic; gateway memory does not balloon.
- **exec-gw idle timeout**: idle bridge dies after configured timeout, `RelayReset` sent on both legs; assert inbound conn survives.
- **exec-gw stream cap**: 33rd concurrent `/bridge/{exe_id}` dial returns 503 with `Retry-After`.
- **Approval filter**: for each of the 5 methods, fake app-server pushes the request; assert response written upstream within 100ms, caller's `ReadJSON` never sees the request frame.
- **REST behavior preservation**: full `turn_api_test.go` and `integration_test.go` suites pass with byte-identical responses before and after PR 3.

## Risks

1. **PR 3 refactor depth**. The broker has tests that assert on specific internal IDs (`conn_turn_test.go:31`); some may need updates if the proxy core uses a different ID-allocation strategy. Mitigation: keep ID allocation policy identical (`atomic.Int64` starting at 1) in the proxy core; tests pass unchanged.
2. **codex-pin bump cadence**. If the pinned tag lags upstream main by months, the soft drift warning becomes noise. Mitigation: include in the bump justfile target a quick "preview diff" step so engineers can decide whether to absorb the upstream changes; recommend monthly review at minimum.
3. **Approval method additions upstream**. Upstream may add a new `requestApproval` variant we miss. The pin's grep check catches this on bump but not in real time. Mitigation: the existing `approvalReply`'s `default:` branch returns `{}` (empty object) which most approval responses accept as a degraded but non-stalling reply.

## Out of scope

- Per-workspace approval policy (confirmed deferred 2026-05-20).
- Exit-code propagation for `codex exec-server` (needs upstream protocol change).
- SSE streaming on REST `/turn` (separate spec if needed).
- Vendoring Go bindings for upstream Rust types (unnecessary with transparent passthrough).
- `opencode-app-gateway` alignment (separate gateway, separate spec).
- Capability downgrade for older codex binary versions (implement only if a real deployment needs it).

## PR 3 deferral note (2026-05-20)

Original migration plan had three PRs. PR 1 (codex-pin + exec-gw hardening) and PR 2 (transparent ws ApprovalFilter + drop bogus `protocolVersion` field) landed all five **functional** gaps from the audit baseline:

| Gap | Closed by |
|---|---|
| 1. No upstream version pinning | PR 1 (`codex-pin.json` + CI lint + `make codex-pin-bump`) |
| 2. exec-gw unbounded ws reads | PR 1 (`MaxFrameBytes` default 16 MiB) |
| 3. exec-gw no idle reaper / stream cap | PR 1 (`BridgeIdleTimeout`, `MaxStreamsPerExecutor`) |
| 4. Transparent ws path didn't auto-reply to approvals | PR 2 (`approvalfilter` package + `OnServerFrame` interceptor in `handleCodexAppWS`) |
| 5a. Broker sent bogus `protocolVersion: "2025-06-18"` | PR 2 (field removed) |
| 5b. Broker has only 2 RPC methods (thread/start + turn/start) | **Intentional**, see below |

PR 3 was originally scoped to do a structural refactor: pivot the REST `/turn` handler to drive the shared transparent-proxy core via an in-process pipe-pair, so the broker's bespoke JSON-RPC client could be deleted. After PR 2 landed `approvalfilter` as a shared package, the actual code duplication this would eliminate is minimal:

- Approval auto-reply logic: **already shared** via `approvalfilter`
- Supervisor lifecycle: **already shared** via `supervisor.EnsureSubprocess`
- The broker's `Conn` / `Pool` / `dispatchFrame` / `Turn` / `StartThread`: these implement a JSON-RPC **client** (sends requests, tracks IDs, waits for responses). The transparent ws path is a JSON-RPC **frame relay** (bidirectional, passes everything through). These are structurally different problems, not duplication.

**Broker's narrow method set (gap 5b) is deliberate, not an oversight.** The REST `/turn` endpoint exists to wrap "send a turn, get the result" in one HTTP call. Callers that need full thread lifecycle (`thread/resume`, `thread/fork`, `thread/list`, `turn/steer`, etc.) should use the transparent `/codex-app/ws` endpoint, which exposes the entire codex v2 protocol with zero gateway code per new method. Expanding REST to mirror all 25+ JSON-RPC methods would re-introduce the maintenance burden that PR 1 + PR 2 were designed to eliminate.

**Decision:** PR 3 as originally scoped is **not done**. The remaining `broker/conn.go` ~420 LOC of JSON-RPC-client code is the right abstraction for the REST adapter's purpose. Refactoring it for "consolidation" would add risk without commensurate benefit.

If future work needs to expose additional thread/turn lifecycle operations to non-ws callers (e.g., HTTP-only clients that can't speak WebSocket), revisit this decision then — the natural extension is to add specific REST handlers for the needed methods, not a generic JSON-RPC-over-HTTP shim.
