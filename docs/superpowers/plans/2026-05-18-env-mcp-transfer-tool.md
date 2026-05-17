# env-mcp `transfer` Tool — Implementation Plan

> Use superpowers:subagent-driven-development. Checkbox-tracked.

**Goal:** add a `transfer` MCP tool to env-mcp that streams a file
or directory between two executors via existing process/* RPCs.

**Spec:** `docs/superpowers/specs/2026-05-18-env-mcp-transfer-tool.md`.

**Tech:** Go, BridgePool, NameResolver — no new external deps.

---

## Task 1 — Streaming helper

**File:** Create `internal/codexappgateway/envmcp/transfer_stream.go`

The byte-pumping core, isolated from MCP plumbing so it can be unit-
tested with stubs.

- [ ] Define interface:
  ```go
  type chunkSource interface {
      Read(ctx context.Context, afterSeq uint64, maxBytes, waitMs int) (chunks []ProcessOutputChunk, nextSeq uint64, exited bool, exitCode *int, failure *string, err error)
  }
  type chunkSink interface {
      Write(ctx context.Context, data []byte) error
  }
  ```
- [ ] Implement `pumpChunks(ctx, src chunkSource, sink chunkSink, maxBytes int) (bytesTransferred int64, srcExitCode *int, err error)`:
  loop process/read → process/write until src.exited; surface
  non-zero src exit as error.
- [ ] Unit tests:
  - happy path: src yields 3 chunks then exits with code 0; pump
    transfers all bytes in order.
  - src exits non-zero with stderr: pump returns error containing
    stderr.
  - sink Write returns error: pump returns it (does not eat).
  - ctx cancellation between chunks: pump returns ctx.Err().
- [ ] Commit `feat(env-mcp): pumpChunks streaming helper for transfer tool`.

---

## Task 2 — TransferTool wiring

**File:** Create `internal/codexappgateway/envmcp/tool_transfer.go`

- [ ] Type:
  ```go
  type TransferTool struct {
      pool     *BridgePool
      resolver *NameResolver
  }
  func NewTransferTool(pool *BridgePool, resolver *NameResolver) *TransferTool
  ```
- [ ] Schema per spec (`src_env`, `src_path`, `dst_env`, `dst_path`,
  optional `recursive`, `timeout_s`).
- [ ] Call body:
  1. Resolve src_env + dst_env to exe_ids via NameResolver.
  2. Acquire src + dst BridgeClients from pool.
  3. Pick src cmd:
     - non-recursive: `["sh","-c","cat <src_path>"]`
     - recursive:    `["sh","-c","tar czf - -C <parent> <basename>"]`
  4. Pick dst tmp path: `<dst_path>.partial-<uuid>` (or for recursive,
     extract into `<parent>` directly — atomicity of dirs is out of scope)
  5. Pick dst cmd:
     - non-recursive: `["sh","-c","cat > <tmp>"]` with `pipe_stdin=true`
     - recursive:    `["sh","-c","tar xzf - -C <parent>"]` with `pipe_stdin=true`
  6. process/start on both.
  7. Build src adapter (chunkSource wrapping `src.Call(process/read)`) +
     dst adapter (chunkSink wrapping `dst.Call(process/write)` with
     base64 wrap).
  8. Run `pumpChunks` under a context bounded by `timeout_s`.
  9. Close dst stdin (process/write with `eof: true` flag — but
     exec-server doesn't have that flag today, so instead call
     `process/terminate` on dst after pump returns ok? No — that
     would lose buffered writes. Right answer: send empty
     process/write with whatever flag closes stdin; check exec-server
     protocol — if no such flag, use a sentinel: `cat` exits on stdin
     EOF, which happens when the ws-side closes the process from the
     write side. Verify by reading the relay protocol.)
  10. wait dst.process/read until `exited` (best effort, bounded by
      remaining ctx).
  11. On dst success: shell rename `mv <tmp> <dst_path>` via a third
      process/start.
  12. On any error: cleanup `rm -f <tmp>` via process/start (best
      effort, separate 5s context).
  13. process/terminate any lingering processes.
  14. Marshal stats; return.
- [ ] Wire into `envmcp.Run` tool list.
- [ ] Commit `feat(env-mcp): transfer tool — file/dir cross-environment copy`.

---

## Task 3 — Tests

**File:** Create `internal/codexappgateway/envmcp/tool_transfer_test.go`

- [ ] Mock chunkSource/chunkSink (already covered in Task 1).
- [ ] Integration-style test using BridgePool with two fakeRelayServers:
  - Server A simulates `cat`-style behavior (returns chunks for
    process/read).
  - Server B simulates `cat > file` (records writes).
  - Drive `TransferTool.Call`; assert bytes match end-to-end and the
    tool result JSON has the expected stats shape.
- [ ] Commit `test(env-mcp): transfer tool end-to-end coverage`.

---

## Task 4 — Docs + chart bump + release

- [ ] Add to README / tool-list doc: brief blurb that `transfer`
  exists.
- [ ] Bump chart to `0.55.0`.
- [ ] Commit, tag, push, CI, pulumi up.
- [ ] Rollout codex-app-gateway only.
- [ ] Smoke test: ask codex to copy a 10 MiB file between two test
  envs; assert success + size match.

---

## Out of scope (v2 candidates per spec)

- HTTP out-of-band transfer for >1 GiB
- Pipelining > 1-deep
- Progress streaming
- Directory atomicity via staging swap
- Checksum verify mode
