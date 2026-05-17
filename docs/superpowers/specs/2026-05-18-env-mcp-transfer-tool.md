# env-mcp `transfer` Tool — Cross-Environment File Transfer

**Date:** 2026-05-18
**Status:** Approved for v0.55.0

## Goal

Give the LLM a single tool call to copy a file or directory from one
remote executor to another (or to/from the same one) without going
through the LLM's context window. Streams through env-mcp via the
existing exec-server `process/start|read|write|terminate` RPCs, so
memory in every gateway hop is bounded by the chunk size (~1 MiB),
not the file size.

## Why

Right now an LLM that wants `hpc-kunshan:/work/dataset.tar.gz` →
`hpc-xian:/scratch/dataset.tar.gz` must either:

a) `read_file` source → `apply_patch` add-file on dest. Pulls the
   entire file through the LLM context. Breaks at >a few MB and
   doesn't handle binary or large directories.
b) Spawn two shells and pipe via `tar | base64 | base64 -d | tar`
   manually. The LLM has to know shell pipeline conventions and
   stitch the calls; output still funnels through the LLM unless
   it uses unidirectional `read_output`+`write_stdin` calls — and
   each chunk still round-trips through the LLM as text.

A first-class `transfer` tool removes both pain points: env-mcp
orchestrates the streaming entirely inside itself.

## Tool surface

```jsonc
// MCP tools/list output (one new entry):
{
  "name": "transfer",
  "description": "Copy a file or directory from src_env to dst_env. Streams in chunks; safe for large/binary files. Atomic at destination (writes to a temp path, renames on success).",
  "inputSchema": {
    "type": "object",
    "properties": {
      "src_env":   {"type": "string", "description": "Source environment name (from list_environments)"},
      "src_path":  {"type": "string", "description": "Absolute path on the source executor"},
      "dst_env":   {"type": "string", "description": "Destination environment name; may equal src_env for same-host copy"},
      "dst_path":  {"type": "string", "description": "Absolute path on the destination executor; parent must exist"},
      "recursive": {"type": "boolean", "description": "Treat src_path as a directory (tar-wrap); preserves mode + symlinks. Default false."},
      "timeout_s": {"type": "integer", "description": "Hard cap on the whole transfer; default 600 (10 min)"}
    },
    "required": ["src_env", "src_path", "dst_env", "dst_path"]
  }
}
```

Result content (one MCPToolContent text element):

```json
{
  "bytes":        12345678,
  "duration_ms":  4321,
  "throughput_mb_per_s": 2.85,
  "dst_path":     "/scratch/dataset.tar.gz"
}
```

On failure: `isError: true` with a single text line describing what went
wrong + which side failed (`src: ...`, `dst: ...`, or `transfer: ...`).

## Wire path

```
                            env-mcp (in app-gateway pod)
                                  │
            ┌─────────────────────┼─────────────────────┐
            │                                           │
       src BridgeClient                            dst BridgeClient
            │                                           │
   process/start "cat <src_path>"           process/start "cat > <tmp_path>"
   (or "tar czf - -C parent base"             (or "tar xzf - -C parent")
   when recursive=true)                       with pipe_stdin=true
            │                                           │
            ▼                                           ▼
       exec-server (src)                       exec-server (dst)
            │                                           │
        cat | stdout                            stdin | dd → file
```

env-mcp loop (single transfer):

```
loop:
  r := src.process/read(after_seq, max_bytes=1 MiB, wait_ms=250)
  for chunk in r.chunks:
      dst.process/write(chunk.payload)   // base64 in, base64 in r
  if r.exited or r.closed: break
src.process/closed → dst.process/write(EOF marker / close stdin)
wait dst.process exited
on success: shell rename .partial → dst_path (atomic on POSIX same-fs)
return stats
```

### Why this shape (not HTTP, not direct peer-to-peer)

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **This (process/* via WS)** | Zero new protocol; works today; bounded memory (chunk size); same auth/cap-token model | RTT-bound throughput (one round-trip per chunk over WAN ≈ 100ms × N) | **v1** |
| HTTP out-of-band (presigned URL on exec-gateway) | True streaming, no base64 30% overhead, HTTP/2 multiplexing | Needs new exec-gateway endpoints, new auth, two new tools (sign-up + sign-down); doubles the surface area | v2 if v1 too slow |
| Direct peer-to-peer (executor ↔ executor via STUN/TURN) | Fastest | NAT traversal, both ends need outbound TCP to a STUN; HPC clusters often forbid this | Not now |

## Pipelining

v1 is **synchronous chunk-by-chunk**: read, then write, then read,
etc. RTT-bound. For a 1 GiB file at 1 MiB chunks and 50 ms RTT to each
side: 1024 × (50 ms read + 50 ms write) ≈ 102 s. Acceptable.

If a user reports this is too slow, v1.1 layer in a 2- or 4-deep
sliding-window pipeline: a producer goroutine fills a bounded channel
of pending `[]byte` chunks from src.process/read, a consumer drains
them into dst.process/write. Channel depth caps memory. Adds ~30 LOC.

## Atomicity at destination

Write into `<dst_path>.partial-<uuid>` then rename to `<dst_path>`. On
success a final `process/start "mv <tmp> <final>"` does the rename;
POSIX same-filesystem rename is atomic. On any failure path:
`process/start "rm -f <tmp>"` cleans up (best-effort — if this also
fails, the partial file lingers but won't be confused for the real
target).

For `recursive=true` with tar, atomicity is per-leaf-file. The tar
extract writes files directly; if it bails mid-stream the destination
is in a partially-extracted state. Documenting this as a known
limitation; properly-atomic dir copies need an intermediate staging
directory + a `mv staging/* final/` swap, which is fiddly. Out of
scope for v1.

## Error handling

End-to-end the `transfer` tool returns one of:

| Condition | Returned shape |
|---|---|
| Both src + dst exited cleanly + rename ok | `isError=false` with stats |
| src process/start failed (file missing, perm denied) | `isError=true`, content = `src: <stderr from cat>` |
| dst process/start failed (no parent dir, perm denied) | `isError=true`, content = `dst: <stderr>` |
| Transfer started, src failed mid-stream (read returns exited with non-zero) | `isError=true`, content = `src: exit_code=N <last stderr>` |
| dst failed mid-stream | `isError=true`, content = `dst: exit_code=N <last stderr>`. tmp file `rm -f` attempted |
| Timeout (timeout_s elapsed) | `isError=true`, content = `transfer: timed out at <bytes> bytes`. Both processes terminate-d, tmp cleanup attempted |
| BridgePool can't dial src_env or dst_env | `isError=true`, content = `environment "X" unavailable: ...` |
| Caller cancels (codex aborts the tool call) | `isError=true`, content = `transfer: cancelled at <bytes> bytes`. Same cleanup as timeout |

All cleanup is best-effort and runs even on context cancellation
(using a `context.Background()` for the cleanup `shell`/`process/terminate` calls
with a 5 s deadline so cancellation actually terminates).

## Memory budget

Per active transfer (per session):
- One outstanding `process/read` response in flight: up to `max_bytes=1 MiB`.
- One outstanding `process/write` request: same size.
- Total per transfer: ~2 MiB on env-mcp side, plus the same on both
  exec-servers' relay buffers.

N concurrent transfers per workspace: N × 2 MiB. The codex app-server
sub-process is the natural limit (one MCP child = one workspace).
Practical cap of ~32 concurrent transfers per workspace before memory
becomes a concern.

## Concurrency

- Two `transfer` calls in parallel on the same env_id: each gets its
  own `process/start` session (unique processId), each runs on its
  own stream_id via BridgePool. Multiplexing handles isolation.
- Two `transfer` calls reading the same src file: no contention; src
  spawns two `cat` processes.
- `transfer` happening concurrently with other tool calls (shell,
  apply_patch) on the same env: no contention; each tool is its own
  process or fs/* RPC.

## Out of scope (v2 candidates)

- HTTP out-of-band path for >1 GiB or low-RTT-optimised transfers.
- Pipelining the chunk loop (above 2-4 deep sliding window).
- True atomic directory copy via staging+swap.
- `transfer` progress events streaming back to the LLM (today's MCP
  doesn't have a partial-results notification; only one final result).
- Resume/checkpoint after disconnect.
- Bandwidth throttling.
- Verifying checksum (could shell out to `sha256sum` on each side
  and compare in a follow-up `verify` mode).

## Test plan

Unit:
- `chunkPump` helper round-trips bytes through fake reader/writer with
  injected delays, asserts ordering + total byte count + EOF
  propagation.
- Error: src process returns non-zero → tool returns `isError=true`
  with correct stderr surfacing.
- Error: dst process write fails → tool surfaces and cleans up tmp.
- Same env_id for src + dst: works (special case nothing).
- Timeout: forced ctx with short deadline → returns timeout error,
  both processes get process/terminate.

Integration (DB-backed, optional in CI):
- Two fake exec-servers via httptest + relay loop; 10 MiB transfer
  end-to-end; assert bytes match + result shape.

## Versioning

Ship as `v0.55.0` (minor — net-new tool, no breaking changes to
existing surface). Only codex-app-gateway needs redeploy.
