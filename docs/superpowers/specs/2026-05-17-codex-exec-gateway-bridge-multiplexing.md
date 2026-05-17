# codex-exec-gateway: Multi-Bridge Multiplexing per Executor

**Date:** 2026-05-17
**Status:** Approved — supersedes the one-bridge-at-a-time invariant in 2026-05-05 spec § "AcquireBridge".

## Authoritative upstream reference

Codex commit `ac466c0dbd` (`feat(exec-server): use protobuf relay frames #22343`, 2026-05-12) introduces the relay protocol with the explicit goal:

> Remote exec-server now needs **one executor websocket to serve multiple harness JSON-RPC sessions**. Rendezvous routes by `stream_id`...
>
> harness and executor endpoints own sequencing, acks, retries, duplicate suppression, segmentation, and reassembly; **rendezvous only routes frames**.

Our codex-exec-gateway IS the "rendezvous". The exec-server side multiplex is implemented in `codex-rs/exec-server/src/relay.rs::run_multiplexed_executor` (lines 295-389 in that commit) — useful as a working reference, but our job is strictly simpler: we route frames, we do not terminate any JSON-RPC session ourselves.

## Goal

Allow **multiple concurrent `/bridge/{exe_id}` sessions** to share a single inbound executor connection, demultiplexed by the relay protocol's `stream_id`. Required so codex's sub-agents (which spawn independent MCP children and therefore independent `BridgeClient`s) can dispatch tools to the same executor in parallel instead of returning 409.

## Why now

codex's multi-agent feature (`spawn_agent`, `spawn_agents_on_csv`) lets one TUI session fan out N parallel sub-agents. Each sub-agent has its own codex context → its own MCP child for our `agentserver` server → its own `BridgePool` → its own `BridgeClient.Get(exe_id)` → its own `wss://.../bridge/{exe_id}` dial.

Today the second concurrent dial gets 409 because `s.registry.AcquireBridge(exeID)` serializes (one bridge session at a time). The original justification was that nhooyr.io/websocket's `Read()` is not safe for concurrent use — two pumps reading the same inbound conn would steal frames from each other.

But the inbound traffic is **already multiplexed at the application layer**. codex's exec-server relay protocol (v0.131-alpha.22+) tags every frame with a `stream_id` (UUID), and each `BridgeClient` picks a unique UUID at dial time (see `internal/codexappgateway/envmcp/bridge.go:84` — `streamID: uuid.NewString()`). exec-server's relay decoder routes incoming frames into per-stream JSON-RPC sessions and emits responses tagged with the same stream_id. So the wire protocol already supports concurrency — we're just throwing it away in the gateway.

## What changes

The transparent pump-pair model becomes a **router** model:

```
Today                                  New
─────                                  ───
bridge[A] ──┐                          bridge[A] ──┐
            ├ AcquireBridge ─→ inbound             ├──→ inbound reader goroutine ──┐
bridge[B] X 409                                    │       (parse, dispatch by sid)│
                                       bridge[B] ──┘                               │
                                                                                   ↓ inbound (one conn)
                                                                                exec-server (already sid-multiplexed)
```

Per executor, we have:
- One inbound ws conn (existing — from `codex exec-server --remote`)
- One reader goroutine that decodes incoming relay frames and dispatches by `stream_id`
- N bridge sessions, each registered under its `stream_id`. Each has its own ws conn back to the env-mcp child.
- Writes to inbound are serialised by a per-inbound mutex (any bridge can write; reader stays distinct)
- Writes to a bridge ws are owned by the inbound reader (one writer per bridge)
- Per-bridge ws: one reader (the existing bridge→inbound pump) + one writer (driven by inbound reader)

## Data model

```go
// inboundConn is per registered executor. Lifecycle: created when the
// /codex-exec/{exe_id} handler accepts a ws, destroyed when the conn
// closes or a new inbound evicts it. Survives across /bridge sessions.
type inboundConn struct {
    exeID      string
    ws         *websocket.Conn
    writeMu    sync.Mutex            // serialise writes to ws

    routesMu   sync.RWMutex
    routes     map[string]*bridgeSession // stream_id → bridge

    closed     chan struct{}
    closeErr   error
}

// bridgeSession is per /bridge/{exe_id} dial. Lifecycle: created when
// the /bridge handler accepts a ws + reads its Resume frame, destroyed
// when either ws closes.
type bridgeSession struct {
    streamID   string
    inbound    *inboundConn
    bridgeWS   *websocket.Conn
    writeMu    sync.Mutex            // serialise writes to bridgeWS
    closed     chan struct{}
}
```

`ConnRegistry` becomes `map[string]*inboundConn` (was `map[string]*websocket.Conn`).

## Bridge handler flow

1. Auth (unchanged): verify cap-token, check workspace ownership, check revoke.
2. Look up `inboundConn` in registry. 503 if missing.
3. Upgrade caller to ws (`bridgeWS`).
4. **Peek first frame**: must be a `RelayMessageFrame` with body `Resume`. Extract `stream_id`. 400 if missing/malformed.
5. Register: `inbound.addRoute(streamID, session)`. If a route for this stream_id already exists (collision — should be UUID-rare), evict the old: close its bridge ws + remove. Log warn.
6. Forward the Resume frame to inbound (so exec-server registers the stream).
7. Start the per-bridge `pumpToInbound` goroutine: read from `bridgeWS`, validate stream_id matches (drop on mismatch — protects against env-mcp bugs), write to inbound under `inbound.writeMu`.
8. Block until either: bridge ws closes, inbound closes, or context cancels.
9. On exit: unregister route, close bridge ws (don't close inbound).

## Inbound handler flow

1. Auth: registration-token check (unchanged).
2. Upgrade to ws.
3. Evict prior inbound for same exe_id if present:
   - Mark old inbound's `closed`; trigger graceful close.
   - All bridge sessions on the old inbound see their `closed` channel fire → their pump exits → they close their own bridge ws.
4. Insert new `inboundConn` into registry; start its reader goroutine.
5. Reader goroutine loop:
   - `ws.Read()` one frame.
   - Decode `RelayMessageFrame` protobuf. Drop with warn on parse error.
   - Look up `routes[frame.StreamId]`. Drop with debug log if missing (legitimate race: bridge just disconnected).
   - Write the entire raw frame to the bridge session's `bridgeWS` under that session's `writeMu`.
   - On `Reset` body: drop the route after forwarding (exec-server signalled end-of-stream).
6. On read error: close `inboundConn`, fan out to all routes (they'll error and exit).

## Write-side serialisation

Two writer-collision cases:
- **N bridges → 1 inbound write**: protected by `inboundConn.writeMu`. Frames are atomic (one ws.Write per relay frame).
- **1 inbound reader → 1 bridge write per route**: only the inbound reader writes to each bridge ws, no concurrency. `bridgeSession.writeMu` is kept for symmetry/future use (e.g., sending Reset on eviction).

## Failure modes & semantics

| Event | Behavior |
|---|---|
| env-mcp opens 2nd bridge for same exe_id | Both succeed concurrently. Each has its own stream_id. |
| User's exec-server reconnects | New inbound evicts prior. All existing bridges fail next read. env-mcp's BridgeClient reconnects per its existing logic (next pool.Get redials). |
| Bridge disconnects mid-call | Route removed. exec-server's next frame for that stream is dropped at gateway (logged at debug). Acceptable — exec-server's relay layer will eventually time out the orphaned stream. |
| Stream_id collision | Second registration evicts first (close + remove). UUID-v4 collision odds are astronomical; this is defensive. |
| Bridge sends non-Resume first frame | 400, close, log warn. |
| Bridge sends frame with wrong stream_id | Drop the frame, log warn. Don't close — could be transient env-mcp state. |
| Inbound dies | All routes get fanout-close. Each bridge handler sees its `closed` channel and exits cleanly. |
| Gateway shutdown | `httpServer.Shutdown` cancels all contexts → readers exit → handlers unwind. |

## Compatibility

- **env-mcp**: no changes. Already sends Resume as first frame and uses per-`BridgeClient` UUID stream_id.
- **exec-server**: no changes. Already runs the relay decoder server-side.
- **Cap-token / auth**: unchanged.
- **Wire protocol**: unchanged.

This is a gateway-internal refactor.

## Migration

Single deploy. No backward-compat layer needed:
- Old gateway + new env-mcp: works (env-mcp always sent Resume; old gateway just forwarded it as part of the pump).
- New gateway + old env-mcp: works only if old env-mcp also sends Resume — and it does, since the relay protocol predates the multiplexing change.

So we can ship this as a single release with no coordinated deploy.

## Out of scope (v2)

- **Stream count quotas**: limit max concurrent streams per inbound to defend against runaway env-mcp spawns. Today's serialisation accidentally bounds it to 1; the new design has no cap. Reasonable upper bound: 64 per executor.
- **Per-stream metrics**: stream open/close, bytes, errors.
- **Stream resumption across inbound reconnects**: today a new inbound = all streams reset. exec-server's relay protocol has `Resume{nextSeq}` for this but env-mcp doesn't currently retain seq, so it would need both ends to cooperate.

## Versioning

`v0.53.0` (minor — meaningful concurrency behavior change). Chart bump, single rollout of codex-exec-gateway only (env-mcp doesn't change).

## Estimated effort

| Piece | LOC | Notes |
|---|---|---|
| Decode `RelayMessageFrame` (vendor proto into exec-gateway) | ~10 import-only | reuse `internal/codexappgateway/envmcp/relaypb` — promote to a shared package |
| `inboundConn` + `bridgeSession` types | ~80 | new file `internal/codexexecgateway/multiplex.go` |
| Rewire `ConnRegistry` | ~40 | replace conn pointer with `*inboundConn` |
| Inbound reader goroutine | ~60 | parse + dispatch |
| Bridge handler updates | ~70 | peek Resume, register, simpler pump |
| Tests | ~200 | unit (route table races), end-to-end (2 concurrent bridges, frames don't interleave wrong) |
| Total | **~460 LOC** | |
