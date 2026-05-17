# copy_path HTTP Out-of-Band Relay

**Date:** 2026-05-18
**Status:** Approved for v0.56.0
**Supersedes:** the cat/SIGTERM byte pump in v0.55.x copy_path (still
the recursive-mode fallback)

## Goal

Move the actual data path of `copy_path` off the env-mcp ⇄ /bridge ⇄
exec-server WebSocket protocol and onto **direct HTTPS streams**
between the two executors and the gateway. env-mcp orchestrates;
neither codex's app-server, env-mcp, nor the relay ws frames touch
the file bytes.

## Why

Current v0.55.x path:

```
exec-server A → bridge ws → exec-gw → bridge ws → env-mcp → bridge ws → exec-gw → bridge ws → exec-server B
                                                ^^^^^^^^^
                                                base64 in JSON-RPC per 1 MiB chunk
                                                serialized read→write→read→write
                                                ~10 MB/s over WAN
```

Wins from HTTP path:

- **No base64**: 33% wire savings
- **No per-chunk RTT**: HTTP body is one TCP stream both ways; throughput = link bandwidth, not RTT-bound
- **No env-mcp memory pressure**: env-mcp orchestrates, doesn't touch bytes
- **Zero new state in app-gateway**: only exec-gateway gains a relay endpoint
- Expected throughput: **10× current** (~100 MB/s on a fast link)

Lose:
- New endpoint surface on exec-gateway (auth, rate limit, lifecycle)
- Requires curl (or wget) on executor — universally available on
  Linux, Windows would need adapter

## Architecture

```
                              codex-exec-gateway pod
                                       │
                  ┌────────────────────┼────────────────────┐
                  │                                         │
                PUT /relay/<ticket>                 GET /relay/<ticket>
                  ▲                                         ▼
                  │                  io.Copy                │
                  │       ┌──────────────────────┐          │
                  │       │  in-memory relay map │          │
                  │       │  (no disk, no fs)    │          │
                  │       └──────────────────────┘          │
                  │                                         │
        (curl PUT, streamed)                  (curl GET, streamed)
                  │                                         │
                  ▼                                         ▼
   ┌───────────────────────┐                  ┌───────────────────────┐
   │  exec-server A        │                  │  exec-server B        │
   │  shell: `cat /src |   │                  │  shell: `curl ... |   │
   │   curl -T- ...`       │                  │   tar xzf - ...`      │
   └───────────────────────┘                  └───────────────────────┘
                  ▲                                         ▲
                  │                                         │
              env-mcp dispatches shell commands; doesn't see bytes
```

env-mcp's role narrows to:

1. Resolve src + dst environment names
2. Mint a relay ticket via the existing exec-gateway internal API
3. Build two shell command lines (one curl PUT, one curl GET) — or
   for recursive, tar-wrap each side
4. Dispatch both via the existing `shell` tool semantics (process/start
   on each executor)
5. Wait for both commands to exit cleanly
6. Rename `.partial` → final on dst
7. Return stats (bytes from the relay endpoint's report; duration from
   wall clock)

## Endpoint design

### Internal: relay ticket mint

Called by env-mcp's app-gateway → exec-gateway internal API channel
(existing `/api/exec-gateway/*` namespace, `X-Internal-Secret` auth).

```
POST /api/exec-gateway/relay/create
  Auth:  X-Internal-Secret
  Body:  {
    "workspace_id":   "ws_xxx",
    "source_exe_id":  "exe_aaa",
    "dest_exe_id":    "exe_bbb",
    "ttl_seconds":    300,      // default 300 (5 min)
    "max_bytes":      0         // 0 = unlimited (workspace-scoped cap applies)
  }
  → 201 Created
  {
    "ticket":       "rly_<32-byte-url-safe-base64>",
    "upload_url":   "https://codex-exec.agent.cs.ac.cn/relay/rly_xxx",
    "download_url": "https://codex-exec.agent.cs.ac.cn/relay/rly_xxx",
    "expires_at":   "2026-05-18T16:05:00Z"
  }
```

Server-side checks: workspace owns BOTH exe_ids (via
`Store.OwnsExecutor`), workspace's active relay count < per-workspace
cap (default 16).

State stored in-memory only: a `map[ticket]*relay` on Server. No DB
write, no cross-pod consistency — tickets are pod-local. With one
replica today this is fine; for HA later, sticky routing via the same
ticket prefix or a Redis-backed store.

### Public: relay endpoint (PUT/GET)

```
PUT /relay/<ticket>
  Auth:  Authorization: Bearer <ticket>   (ticket IS the auth)
  Body:  streamed bytes (Transfer-Encoding: chunked)
  → blocks until matching GET claims its side (or 408 after ttl)
  → 200 OK with body  `{"bytes": 12345678, "status": "ok"}`
  → 410 Gone if ticket already consumed
  → 423 Locked if another PUT already in-flight on this ticket

GET /relay/<ticket>
  Auth:  Authorization: Bearer <ticket>
  → blocks until matching PUT (or 408)
  → 200 OK; body = streamed bytes (Transfer-Encoding: chunked)
  → 410 / 423 same as above
```

A single ticket consumes exactly one PUT + one GET. After both succeed
(or either fails), the ticket is removed from the map.

### Pairing goroutine

Spawned at ticket creation. Pseudo-Go:

```go
func (r *relay) run() {
    defer func() { r.cleanup(); close(r.done) }()
    ctx, cancel := context.WithTimeout(context.Background(), r.ttl)
    defer cancel()

    var src io.Reader
    var dst io.Writer
    var dstFlusher http.Flusher

    // Whichever side shows up first, wait for the other.
    for src == nil || dst == nil {
        select {
        case put := <-r.putCh:
            if src != nil { put.respond(http.StatusLocked); continue }
            src = put.body; r.putResp = put
        case get := <-r.getCh:
            if dst != nil { get.respond(http.StatusLocked); continue }
            dst = get.writer; dstFlusher = get.flusher; r.getResp = get
        case <-ctx.Done():
            r.timeoutBothSides()
            return
        }
    }

    // Both sides connected — stream.
    r.bytes, r.copyErr = io.Copy(flushingWriter{dst, dstFlusher}, src)
    // Close GET first (sends final chunk + EOF), then ack PUT.
    r.getResp.finish(r.copyErr)
    r.putResp.respond(http.StatusOK, jsonStats(r.bytes))
}
```

`flushingWriter` calls `Flush()` after each Write so the GET side
actually streams instead of waiting for buffer to fill.

### Auth on /relay/

The Bearer ticket IS the credential. Once minted, anyone with the
ticket can connect either side. We rely on env-mcp being the only
holder + the short TTL + single-use semantics.

Workspace boundary is enforced at mint time (env-mcp verified the
workspace owns both exe_ids before asking for the ticket); the relay
endpoint itself doesn't re-check because it has no notion of which
exe_id is calling — both sides hit the same URL with the same
ticket.

Concretely: a malicious workspace can't poach another workspace's
relay because (a) they need the ticket, (b) the ticket is opaque
random, (c) it expires in 5 min, (d) it's single-use.

## env-mcp orchestration

`copy_path` v2 flow (for `recursive=false`):

```
exeIdA = resolver.Resolve(source_env)
exeIdB = resolver.Resolve(dest_env)

// 1. Mint relay ticket
ticket, urls := execGwClient.CreateRelay(workspace, exeIdA, exeIdB, ttl=600s)

// 2. Build shell commands
srcCmd := fmt.Sprintf(
    "curl -fsS --upload-file %s -H 'Authorization: Bearer %s' %s",
    shQuote(source_path), ticket, urls.upload,
)
tmp := dest_path + ".partial-" + xferID
dstCmd := fmt.Sprintf(
    "curl -fsS -H 'Authorization: Bearer %s' %s -o %s",
    ticket, urls.download, shQuote(tmp),
)

// 3. Dispatch both shells in parallel (process/start each, then poll
//    process/read until exit, on both bridge clients).
var srcWg, dstWg sync.WaitGroup
srcWg.Add(1); dstWg.Add(1)
go func() { defer srcWg.Done(); srcExitCode = runShell(srcBC, srcCmd) }()
go func() { defer dstWg.Done(); dstExitCode = runShell(dstBC, dstCmd) }()
srcWg.Wait(); dstWg.Wait()

// 4. Validate both exited zero. On any non-zero: surface stderr +
//    rm -f tmp. Curl's --fail-with-body gives us HTTP error info.

// 5. Rename .partial → final via process/start mv on dst.

// 6. Stats: env-mcp can also call GET /api/exec-gateway/relay/<ticket>/stats
//    or rely on the dst curl's bytes-transferred output.
```

For `recursive=true`:
```
srcCmd = "tar czf - -C %s %s | curl ..."
dstCmd = "curl ... | tar xzf - -C %s"
```

Pipefail enabled (`set -o pipefail`) so curl/tar failures surface as
the shell's exit code.

## Failure modes

| Event | Result |
|---|---|
| Src curl errors before PUT lands | Src shell exit non-zero, dst GET times out (408) → dst shell exit non-zero too. env-mcp surfaces src stderr. |
| Dst curl errors after GET starts | io.Copy errors mid-stream → dst gets truncated body → tar/cat fails on dst → dst shell exit non-zero. env-mcp surfaces dst stderr + rm tmp. |
| Network drop mid-transfer | One side's HTTP errors → relay goroutine sees io.Copy error → both responses close. Same surface as above. |
| Ticket TTL expires while waiting | Pairing goroutine times out → both pending sides get 408 → curl exits non-zero. |
| curl not installed on executor | Shell exits 127 with "curl: command not found" — env-mcp can detect this and fall back to v0.55.x cat-based path (one toggle in CopyPathTool). |
| relay pod restart mid-stream | Both curls fail. env-mcp surfaces. No partial file (mv hasn't happened yet). |

## Memory / resource budget

Per active relay:
- Map entry: ~512 B
- 2 chan struct: ~256 B
- io.Copy buffer: 32 KB (one-time, on stack)
- TCP buffers: ~64 KB per direction

Per pod cap: **64 concurrent relays** → ~10 MB total memory.

Workspace cap: **16 concurrent** → bound a runaway agent.

## Backward compatibility

copy_path tool surface unchanged. Internal switch:

- v0.56.0 first ships HTTP path enabled by default
- If src or dst shell returns exit 127 (curl missing) within ~1s of
  start, env-mcp transparently re-runs with the v0.55.x cat-based
  path. Toggle in CopyPathTool config.
- An env override `CXG_COPY_PATH_TRANSPORT=ws|http|auto` (default
  `auto`) lets operators force one path for debugging.

The cat path remains in the codebase but only triggered as fallback
or when explicitly requested.

## Out of scope (v2)

- Resume / range support (HTTP Range header on GET would let curl
  resume a partial download — currently re-transmits from scratch).
- Multi-writer fan-out (one PUT, N GETs to different executors).
- Checksum verification end-to-end (could add `--checksum sha256` arg
  that wraps both sides with `tee >(sha256sum)`).
- Throttling / bandwidth cap.
- Sticky routing for multi-replica gateway HA.

## Test plan

Unit (no curl required):
- relay map: concurrent mint/lookup/expire
- pairing goroutine: PUT-first, GET-first, both-arrive-simultaneously,
  ttl-timeout
- workspace ownership rejects cross-workspace mint

Integration:
- httptest.Server hosting the relay endpoints + a fake `curl -T-`
  using net/http; round-trip a 10 MiB payload; assert byte equality
  and stats
- Failure path: src side gets a body of N bytes then drops the
  connection → dst sees truncated body + io.Copy error → relay
  returns 500 with error JSON

End-to-end (manual smoke):
- Same as today's copy_path test: copy /etc/hostname between two
  HPC executors, then sha256 both sides.

## Versioning

`v0.56.0` — meaningful new infra (HTTP relay endpoint). Both gateways
need redeploy (exec-gateway for the relay; app-gateway for env-mcp
that calls it).
