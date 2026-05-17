# copy_path HTTP Out-of-Band Relay — Implementation Plan

> Use superpowers:subagent-driven-development. Checkbox-tracked.

**Goal:** add HTTPS relay endpoint on codex-exec-gateway + rewrite
env-mcp's copy_path to use it (with cat-pump fallback).
Spec: `docs/superpowers/specs/2026-05-18-copy-path-http-relay.md`.

---

## Task 1 — Relay registry + types

**File:** Create `internal/codexexecgateway/relay/relay.go`.

- [ ] Type:
  ```go
  type Relay struct {
      Ticket        string
      WorkspaceID   string
      SourceExeID   string
      DestExeID     string
      ExpiresAt     time.Time
      MaxBytes      int64

      putCh chan putReq
      getCh chan getReq
      done  chan struct{}

      bytes int64
      err   error
  }
  type putReq struct { body io.Reader; respond func(status int, body []byte); done chan struct{} }
  type getReq struct { writer io.Writer; flusher http.Flusher; done chan struct{} }
  ```
- [ ] `type Registry struct { mu sync.Mutex; m map[string]*Relay; workspaceCount map[string]int }`
- [ ] Methods:
  - `Create(workspaceID, src, dst string, ttl time.Duration, maxBytes int64) (*Relay, error)`
    - mints ticket = `"rly_" + base64url(32 random bytes)`
    - returns error if workspace already has 16 active
    - spawns pairing goroutine
  - `Lookup(ticket string) (*Relay, bool)`
  - `delete(ticket string)` — internal, called by goroutine on done
- [ ] Pairing goroutine `run()`: waits for both sides + io.Copy + cleanup.
- [ ] `flushingWriter` adapter that calls Flush() after each Write.
- [ ] Unit tests: PUT-first, GET-first, ttl expiry, double-PUT 423,
  workspace cap.
- [ ] Commit `feat(codex-exec-gateway): in-memory relay registry for HTTP byte-pump`.

---

## Task 2 — Relay HTTP handlers + routes

**File:** Create `internal/codexexecgateway/relay/handlers.go`.

- [ ] `handleRelayPut` and `handleRelayGet`:
  - Extract ticket from URL + Authorization Bearer (must match).
  - Lookup registry; 410 if absent.
  - Build req struct, send on chan, block on its `done`.
  - On done: write response status + body (PUT side gets JSON stats,
    GET side gets streamed body).
- [ ] Wire in `server.go`:
  ```go
  r.Put("/relay/{ticket}", relayHandlers.HandlePut)
  r.Get("/relay/{ticket}", relayHandlers.HandleGet)
  ```
- [ ] Internal-API mint route under `/api/exec-gateway/relay/create`:
  - Verify `X-Internal-Secret`.
  - Body: `{workspace_id, source_exe_id, dest_exe_id, ttl_seconds, max_bytes}`.
  - Call `store.OwnsExecutor` for both exe_ids (separate checks).
  - Call `registry.Create`; on success respond `{ticket, upload_url, download_url, expires_at}`.
  - `upload_url` / `download_url` derived from `s.config.PublicHTTPSBaseURL` (new config); both URLs are the same (`<base>/relay/<ticket>`) — clients use PUT for upload, GET for download.
- [ ] Tests: PUT 410 on bad ticket; GET 410 on bad ticket; round-trip via httptest.
- [ ] Commit `feat(codex-exec-gateway): /relay PUT+GET endpoints + /api/.../relay/create mint`.

---

## Task 3 — Config + ServeConfig knobs

**Files:** `internal/codexexecgateway/config.go`, helm values, pulumi stack.

- [ ] Add to `Config`:
  ```go
  PublicHTTPSBaseURL string // e.g. "https://codex-exec.agent.cs.ac.cn"
  RelayDefaultTTL    time.Duration // default 5m
  RelayMaxPerWorkspace int          // default 16
  ```
- [ ] LoadConfigFromEnv reads `CEG_PUBLIC_HTTPS_BASE_URL`, defaults to empty (env-mcp falls back to ws path).
- [ ] Helm chart `codex-exec-gateway-deployment.yaml` adds env var from values: `codexExecGateway.publicHttpsBaseUrl`.
- [ ] Pulumi stack sets it to `"https://codex-exec.agent.cs.ac.cn"` for nj-prod.
- [ ] Commit `feat(codex-exec-gateway): config for HTTPS relay public URL + per-workspace cap`.

---

## Task 4 — env-mcp: relay-aware CopyPathTool

**File:** Rewrite `internal/codexappgateway/envmcp/tool_copy_path.go`.

- [ ] Add `RelayClient` to envmcp (subset of agentserver-side
  ExecGatewayClient — only needs CreateRelay). Pass loopback URL +
  workspace + cap-token. Or extend the existing http call path used
  by NameResolver / loopback.
- [ ] `CopyPathTool` gains a `transport` field: `auto` (default), `http`, `ws`.
- [ ] New flow (transport=http):
  1. Resolve src/dst names.
  2. Call gateway's `/api/exec-gateway/relay/create` (via app-gateway loopback /internal/connected pattern, but a new endpoint or by adding the create call to the loopback API).
     - **Simpler**: env-mcp doesn't go through loopback — instead, the
       codex-app-gateway pod has direct internal access to
       codex-exec-gateway. env-mcp's pool already dials there for /bridge.
       Add a small helper to env-mcp's pool that POSTs to
       /api/exec-gateway/relay/create on the same exec-gateway base URL
       (HTTP variant of WS).
     - Auth via X-Internal-Secret (env-mcp gets it from cap-token env or
       a new dedicated env var passed at spawn).
  3. Build src + dst shell command lines with curl.
  4. Dispatch both shells in parallel via process/start + poll.
  5. Wait both exits; check exit codes.
  6. If either is 127 (curl missing) AND `transport=auto`, fall back to ws-pump path.
  7. `mv .partial → final` on dst.
  8. Return stats (bytes from the gateway's response on PUT, duration from wall clock).
- [ ] Preserve ws-pump path for `recursive=true` + transport=ws explicit + curl-absent fallback. Keep the existing `pumpChunks` + helpers, just gate them behind a branch.
- [ ] Tests: with httptest stand-in for the relay endpoint, verify the orchestration + exit-code propagation. Pure ws-pump tests remain.
- [ ] Commit `feat(env-mcp): copy_path uses HTTPS relay (cat-pump as fallback)`.

---

## Task 5 — Wire INTERNAL_SHARED_SECRET into env-mcp env

**Files:** `internal/codexappgateway/server.go` SpawnConfig,
`internal/codexappgateway/envmcp/envmcp.go` RunArgs,
`internal/codexappgateway/codexhome/codexhome.go` config emission.

- [ ] Plumb the exec-gateway internal shared secret through env-mcp's env so it can POST to /api/exec-gateway/relay/create.
- [ ] Add CLI flag `--exec-gateway-internal-secret-env` + env var.
- [ ] codexhome.go writes the new env var into the agentserver MCP entry's env block.
- [ ] Commit `chore(env-mcp): plumb exec-gateway internal-shared-secret for relay create`.

---

## Task 6 — Tests + chart bump + release v0.56.0

- [ ] Final go test ./... clean.
- [ ] Bump chart to 0.56.0.
- [ ] Commit, tag, push, CI.
- [ ] Pulumi up (preview first; user authorizes).
- [ ] Rollout codex-exec-gateway + codex-app-gateway.
- [ ] Smoke test: same as v0.55.x — copy_path /etc/hostname between
  two hpc-* envs; assert success + bytes match. Inspect gateway logs
  for `relay: paired ticket=...` line confirming HTTP path was used.
- [ ] Negative smoke: temporarily unset CEG_PUBLIC_HTTPS_BASE_URL and
  verify copy_path falls back to ws-pump (no env-mcp redeploy needed,
  just check logs).

---

## Out of scope (record for v2)

- Resume via HTTP Range
- Multi-writer fan-out
- Checksum verify mode
- Bandwidth throttling
- Sticky-routing for HA exec-gateway replicas
