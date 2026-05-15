> **SUPERSEDED 2026-05-15.** The supervisor key + inbound auth model are
> superseded by `2026-05-15-codex-app-gateway-oauth-bridge-design.md`.
> The subprocess lifecycle, S3 round-trip, ws frame proxy, and reaper
> sections of THIS document remain in force.

# codex-app-gateway as a thin proxy around `codex app-server` subprocesses

**Status:** draft (central technical claim PoC-validated 2026-05-10, see § PoC log)
**Date:** 2026-05-10
**Owner:** agentserver / codex integration
**Refines:** [`2026-05-10-codex-gateway-mcp-rewrite.md`](2026-05-10-codex-gateway-mcp-rewrite.md)
   — replaces only that spec's **Subsystem 2** (codex-app-gateway runtime).
   Subsystems 1 (no fork patches), 3 (codex-exec-gateway), and 4
   (env-mcp) are unchanged and remain the source of truth in the
   parent spec.

## Why this refinement

The 2026-05-10 MCP-rewrite spec plans codex-app-gateway as a Go service
that hand-implements ~17 codex app-server v2 RPCs (initialize / thread/* /
turn/* + 8 ServerNotification mappings) and spawns `codex exec
--experimental-json` as a short-lived subprocess per turn.

Two facts make that plan heavier than necessary:

1. **Upstream codex 0.130.0 ships `codex app-server --listen ws://IP:PORT`** — a
   complete v2 RPC endpoint. The codex TUI's existing `--remote <ADDR>`
   flag dials directly into it. Our gateway can be a transparent
   ws-frame-level proxy between the user's TUI ws and a per-thread
   `codex app-server` subprocess instead of re-implementing the protocol.
2. **PoC #3 (2026-05-10, this spec)** validated that:
   - A loopback ws connection to `codex app-server --listen ws://...`
     handles `initialize → thread/start → turn/start` end-to-end and
     streams every expected `ServerNotification` (`thread/started`,
     `thread/status/changed`, `turn/started`, `item/started`,
     `item/completed`, `turn/completed`).
   - Two concurrent `codex app-server` instances with different
     `CODEX_HOME` dirs operate in full isolation (no sqlite lock
     contention, independent thread ids).

Net effect on Subsystem 2:

| Concern | MCP-rewrite spec | This refinement |
|---|---|---|
| codex-app-gateway role | Implements 17 RPCs in Go + per-turn `codex exec` spawner | Terminates user JWT + per-thread `codex app-server` subprocess manager + transparent ws frame proxy |
| Per-user-thread codex processes | N short-lived (one `codex exec` per turn) | 1 long-lived `codex app-server` per active thread |
| Postgres tables we own | `codex_threads`, `codex_turns`, `codex_turn_events` (hand-rolled) | **Deleted.** codex's own sqlite state DB inside CODEX_HOME owns thread state. We back up the per-thread CODEX_HOME tarball to S3. |
| Estimated Go LOC for codex-app-gateway | ~1500 (handlers + store + runner + event_mapper + recovery + persistence) | ~400 (auth + supervisor + frame proxy + S3 setup/teardown) |
| Tracking new upstream RPCs (skills, plugins, mcpServer/*, …) | We must implement each one | Inherited from subprocess upgrade |
| env-mcp design (PR #78, Subsystem 4) | Spawned per turn by us | Spawned per turn **by the codex subprocess** via the `[mcp_servers]` config we write into its CODEX_HOME |

## Architecture

```
Local laptop                                agentserver pod
─────────────                                ─────────────────────────────────────────
codex --remote wss://AS/codex-app/...
       │
       │  codex app-server v2 JSON-RPC (no jsonrpc:"2.0" field, just id/method/params/result|error)
       │  initialize / thread/start / turn/start / ...
       ▼
┌────────────────────────────────────────────────────────────────────────────┐
│  codex-app-gateway (Go) — auth + per-thread subprocess manager + ws proxy   │
│                                                                            │
│  1. ws upgrade: validate user JWT (existing wstoken)                       │
│  2. Read inbound `initialize` → snoop `clientInfo` only (don't translate)   │
│  3. Inbound `thread/resume {threadId}` or `thread/start {}`:                │
│     - look up workspace's mapping `thread_id → subprocess`                  │
│     - if no live subprocess: spawn one (see § Spawn)                        │
│     - if subprocess exists for another connection: reject (one-tui-per-     │
│       thread phase 1 invariant)                                             │
│  4. Open one ws connection from gateway → subprocess on loopback            │
│  5. Run two pumps (frame-level, like codex-exec-gateway/bridge):            │
│        userTUI.ws  ⇄  subprocess.ws                                         │
│     Auth checked once per side; forwarding is byte-for-byte thereafter.    │
│  6. On either side close: close the other; if subprocess still alive,      │
│     leave it idle (idle-shutdown reaper handles GC; see § Lifecycle).      │
└────────────────────────────────┬───────────────────────────────────────────┘
                                 │ loopback ws (ws://127.0.0.1:RND)
                                 ▼
                  ┌──────────────────────────────────────────────────┐
                  │ codex app-server subprocess (1 per active thread)│
                  │   --listen ws://127.0.0.1:0                      │
                  │   CODEX_HOME=/tmp/cag/<workspace_id>/<thread_id>/│
                  │     ├─ config.toml      (we write per-turn:      │
                  │     │   [features] shell_tool=false, …           │
                  │     │   [mcp_servers.exe_xxx] command=env-mcp)   │
                  │     ├─ sessions/<thread_id>.jsonl                │
                  │     └─ <state sqlite, etc.>                      │
                  │ Implements all 17 RPCs natively.                 │
                  │ Spawns env-mcp child(ren) per turn via mcp_servers│
                  │ config (Subsystem 4).                            │
                  └──────────────────────────────────────────────────┘
```

## Phase-1 RPC surface

Same 17-RPC inventory as the MCP-rewrite spec, but **we don't implement
any of them** — they are forwarded byte-for-byte to the subprocess.
Whatever subset of v2 codex's app-server actually supports is what the
TUI sees.

The phase-1 codex-app-gateway never parses inbound JSON-RPC frames
beyond a thin pre-handshake snoop on `initialize` (to record
`clientInfo` for diagnostics) and a reactive parse of `thread/start`
result and `thread/resume` params (to learn the `thread_id` and pick
the right subprocess). All other frames pass through opaque.

This is the same operational model as `codex-exec-gateway`'s `/bridge/{exe_id}`
endpoint — the gateway is a frame-level forwarder, not a protocol
translator.

## Subprocess lifecycle (§ Spawn / § Idle / § Resume)

### Spawn

When the first TUI session for a `(workspace_id, thread_id)` connects:

1. Mint a per-thread `CODEX_HOME` tmpdir under
   `/tmp/codex-app-gateway/<workspace_id>/<thread_id>/`.
2. If S3 has a saved tarball for this `(workspace_id, thread_id)`,
   download and untar it into `CODEX_HOME`. (Resume.)
3. Otherwise, seed `CODEX_HOME/config.toml` with the workspace's
   default config: `model_provider`, `model`, `[features]` (with
   `shell_tool=false`, `unified_exec=false`, `apply_patch_freeform=false`
   per the MCP-rewrite spec § Subsystem 2 deltas), and the per-turn
   `[mcp_servers.exe_*]` entries built from the workspace's bound
   executors (see Subsystem 3 of the parent spec).
4. Spawn:

   ```
   codex app-server --listen ws://127.0.0.1:0
   ```

   with `CODEX_HOME=<tmpdir>` and the workspace's `OPENAI_API_KEY` /
   `CODEX_API_KEY` in the env. Capture stdout's first line — codex
   prints `ws://IP:PORT` — to learn the bound port.

5. Wait for `http://127.0.0.1:PORT/readyz` to 200 (codex app-server
   exposes this) — typically <1s after the listen line is printed.
6. Record `(workspace_id, thread_id) → {pid, port, codex_home}` in an
   in-memory map.

### Per-turn config refresh

The `[mcp_servers]` block reflects the workspace's currently-bound
executors. Phase 1 keeps it static for the subprocess's lifetime. If an
executor binding changes mid-thread:

- **Phase 1 behavior:** the subprocess keeps using the old set. A
  `thread_id` is "pinned" to its initial executor set until the
  subprocess restarts.
- **Phase 1 escape hatch:** the gateway exposes an admin
  `POST /api/codex-app-gateway/threads/{thread_id}/restart` that
  gracefully drains + respawns the subprocess (S3 round-trip is
  automatic). Operator can call this after binding changes.
- **Phase 2 candidate:** use codex's runtime `mcpServer/add` /
  `mcpServer/remove` v2 RPCs (currently in MCP-rewrite spec § non-goals).
  Avoids subprocess restart but couples us to upstream RPC schema.

### Idle shutdown

A per-subprocess idle timer (default 30 min, configurable
`CXG_IDLE_SHUTDOWN`) fires when:

- No active TUI ws connection.
- No active inbound RPC frame for the timer window.

On fire:

1. Send `SIGTERM` to the subprocess. Wait up to 10s for graceful exit.
2. After exit (or `SIGKILL` on timeout): tar `CODEX_HOME` and upload to
   S3 at `s3://codex-app-gateway/<workspace_id>/<thread_id>.tar.zst`.
3. Remove the local tmpdir. Drop the entry from the in-memory map.

If the gateway pod restarts, the in-memory map is empty; the next TUI
connection for any thread spawns a fresh subprocess and pulls from S3.

### Concurrent connection handling (single-tui-per-thread invariant)

If a second TUI ws tries to attach to a `(workspace_id, thread_id)`
that already has a live subprocess + active gateway-side ws connection:

- **Phase 1:** reject with `409 Conflict / "thread busy"`. Operator can
  hard-kick via the restart endpoint above.
- **Phase 2 candidate:** support multi-subscriber via codex's
  `cursor`-based subscription (the v2 protocol has the bones for this);
  defer.

This invariant simplifies the proxy to a 1-to-1 ws pair with no
fan-out logic.

## State management

| Store | Owner | Per-thread shape |
|---|---|---|
| sqlite state DB inside CODEX_HOME | codex itself | All thread/turn/event records, agent state, etc. |
| `sessions/<thread_id>.jsonl` inside CODEX_HOME | codex itself | The full session log codex writes |
| S3 tarball | gateway | `s3://codex-app-gateway/<workspace_id>/<thread_id>.tar.zst` containing the entire CODEX_HOME tree |
| In-memory map | gateway | `(workspace_id, thread_id) → {pid, port, codex_home, last_active_at}` |

Postgres tables that the MCP-rewrite spec planned to add
(`codex_threads`, `codex_turns`, `codex_turn_events`) are **not
needed** under this refinement. Any list-threads UX can be served by
listing S3 keys plus the in-memory active-set.

If the workspace requires additional auditability (who-did-what
indexing) beyond what S3 keys provide, add a single thin indexing
table later — but not in phase 1.

## Auth model (delta only)

| Hop | Credential | Validator | Lifetime |
|---|---|---|---|
| TUI → codex-app-gateway ws upgrade | per-user JWT bearer (existing wstoken issuance) | gateway HTTP middleware | per session, refreshable |
| codex-app-gateway → codex subprocess loopback ws | none (loopback is trusted within the pod) | n/a | n/a |
| codex subprocess → llmproxy | workspace OPENAI_API_KEY (existing) | llmproxy | short-lived |
| codex subprocess → env-mcp child stdio | none (parent-child) | n/a | n/a |
| env-mcp child → codex-exec-gateway/bridge | per-turn HMAC capability token | exec-gateway (unchanged from PR #78) | turn duration + 1h |

Loopback trust is acceptable because:

- Gateway and subprocess share the same Linux namespace inside the
  pod; only the gateway process can connect to `127.0.0.1:PORT`.
- The subprocess listens on `127.0.0.1` only (codex enforces this on
  `--listen ws://127.0.0.1:0`).

If we ever split gateway and subprocess into separate pods/hosts,
flip on `--ws-auth signed-bearer-token --ws-shared-secret-file <path>`
on the subprocess side and have the gateway mint a short-lived JWT per
ws-pair. The codex flag inventory is already there (`--ws-issuer`,
`--ws-audience`, `--ws-max-clock-skew-seconds`).

## Repository layout (delta vs MCP-rewrite spec)

```
agentserver/
├── cmd/codex-app-gateway/main.go    (already shipped in PR #78; serve subcommand to be wired)
├── internal/codexappgateway/
│   ├── envmcp/                       (already shipped in PR #78)
│   ├── server.go                     (NEW: chi routes + ws upgrade + JWT middleware)
│   ├── supervisor/
│   │   ├── supervisor.go             (NEW: spawn / health-check / kill, in-memory map)
│   │   ├── codex_home.go             (NEW: tmpdir mgmt + S3 round-trip + config.toml writer)
│   │   └── idle_reaper.go            (NEW: idle-shutdown ticker)
│   ├── proxy/
│   │   └── ws_proxy.go               (NEW: bidirectional ws frame pump; mostly reuses
│   │                                   internal/wsbridge/ which Subsystem 3 also consumes)
│   └── server_test.go / supervisor_test.go / proxy_test.go
└── Dockerfile.codex-app-gateway      (extend: copy `codex` binary into runtime stage so
                                       the subprocess can be spawned; pin codex version)
```

Files that the MCP-rewrite spec listed and that this refinement deletes:

- `internal/codexappgateway/protocol/{client_request,server_notification,types,...}.go`
- `internal/codexappgateway/handlers/{initialize,thread,turn,dispatch,...}.go`
- `internal/codexappgateway/runner/{runner,event_mapper,mcpconfig}.go`
- `internal/codexappgateway/store.go` + `migrations/`
- `internal/codexappgateway/session_worker.go`
- `internal/codexappgateway/connregistry/`

Net: ~1100 fewer lines of Go to write/maintain.

## What changes in env-mcp (Subsystem 4)

Nothing. PR #78 is a self-contained binary subcommand
(`codex-app-gateway env-mcp ...`) that takes its inputs from CLI flags
+ env vars. Whoever writes the `[mcp_servers]` config that spawns it —
under the MCP-rewrite spec the gateway, under this refinement the
codex subprocess via the gateway-written config — gets the same
behavior.

## Phase 1 vs deferred

**Phase 1 (this refinement):**

- `codex-app-gateway serve` HTTP/WS endpoint with user-JWT auth.
- Per-thread subprocess supervisor (spawn, health-check, idle-shutdown,
  S3 tar round-trip).
- Transparent ws frame proxy.
- Single-TUI-per-thread invariant; admin restart endpoint.
- Reuses `internal/wsbridge/` (extracted as a cross-Subsystem
  prerequisite per the MCP-rewrite spec § Repository layout).
- Subsystem 3 (codex-exec-gateway) and Subsystem 4 (env-mcp PR #78)
  unchanged.

**Phase 2 candidates (out of scope):**

- Multi-TUI subscriber per thread (cursor-based subscription).
- Runtime `mcpServer/add` / `mcpServer/remove` for executor-binding
  changes without subprocess restart.
- Cross-pod `codex app-server` (subprocess on a different pod from the
  gateway), gated by `--ws-auth signed-bearer-token`.
- Long-running thread state hot-reload (e.g. CODEX_HOME schema migration
  across codex versions).
- Subprocess affinity for stickiness across gateway pod restarts (avoid
  cold spawn on the next TUI reconnect).

## Open risks

1. **`codex app-server` is `[experimental]` upstream.** RPC shape may
   shift between codex versions. Mitigation: pin codex version in the
   gateway Dockerfile; add an e2e schema-drift test that reads the
   subprocess's `tools/list` (or equivalent) and asserts the wire shape
   we expect.

2. **CODEX_HOME size and S3 round-trip cost.** Each subprocess's CODEX_HOME
   contains sqlite + sessions/ + skills/ + memories/ + state files.
   PoC tarball size on a fresh CODEX_HOME with one 5-turn session was
   ~600 KB (compressed `.tar.zst`); production threads could grow
   bigger. Mitigation: profile after first deployment; if a single
   thread's tarball exceeds e.g. 50 MB, prune old session jsonl
   before tar (codex tolerates partial/missing rollouts).

3. **`thread/resume` semantics.** The TUI sends `thread/resume` when
   reconnecting to a known thread. Our gateway has to dispatch this to
   the right (possibly newly-spawned) subprocess. The PoC didn't yet
   exercise the resume path; phase-1 e2e must.

4. **Subprocess crash mid-turn.** If `codex app-server` crashes while a
   turn is in flight:
   - The TUI's open ws gets an EOF.
   - Our supervisor detects the exit and removes the in-memory map
     entry.
   - The next TUI reconnect will spawn a fresh subprocess from S3 —
     **but** the in-flight turn's state is whatever was committed to
     CODEX_HOME's sqlite at crash time (probably an interrupted
     `running` row). We need to verify codex's resume code handles
     that gracefully (it should, since this is a normal codex use case).

5. **Per-pod thread density.** Each active subprocess holds one
   inotify watcher tree (sessions/), some sqlite handles, and a
   long-running tokio runtime. Empirical density unknown until we
   load-test. Easy escape hatch: shard by `thread_id` hash across
   multiple gateway pods.

## PoC log (2026-05-10)

### PoC #3 — `codex app-server` v2 RPC end-to-end

Ran against `codex-cli 0.130.0` (upstream binary, no fork patch).

```bash
mkdir -p /tmp/codex-appserver-poc
codex app-server --listen ws://127.0.0.1:9001 \
    > /tmp/codex-appserver-poc/server.log 2>&1 &
# server prints "codex app-server (WebSockets) / listening on: ws://127.0.0.1:9001 / readyz: ..."

# python ws client (compression=None to avoid permessage-deflate, same gotcha
# as MCP-rewrite spec § Subsystem 4):
#   send: initialize, initialized, thread/start, turn/start "Say only the word: pong"
#   recv: initialize result with userAgent/codexHome/platformOs,
#         thread/start result with thread.id,
#         turn/start result with turn.status=inProgress,
#         then ServerNotifications:
#           remoteControl/status/changed
#           thread/started
#           thread/status/changed (idle → active)
#           turn/started
#           item/started   (userMessage)
#           item/completed (userMessage)
#           error: 401 from upstream LLM (user's API key expired — irrelevant
#             to this PoC; it proves the proxy/RPC layer reached the LLM call)
#           thread/status/changed (active → systemError)
#           turn/completed (status=failed)
```

**What this proves:**

- ✅ Loopback ws to `codex app-server --listen` accepts unauthenticated
  connections (no `--ws-auth` needed for in-pod IPC).
- ✅ The full `initialize → thread/start → turn/start → ServerNotification
  stream → turn/completed` lifecycle works with codex's own RPC
  implementation.
- ✅ The wire format matches what codex's TUI expects on the other end
  (both peers use the same upstream binary's protocol code path).

### PoC #3b — concurrent per-CODEX_HOME isolation

```bash
mkdir -p /tmp/codex-appserver-poc/home_a /tmp/codex-appserver-poc/home_b
cp ~/.codex/config.toml /tmp/codex-appserver-poc/home_{a,b}/config.toml
CODEX_HOME=/tmp/codex-appserver-poc/home_a codex app-server --listen ws://127.0.0.1:9002 &
CODEX_HOME=/tmp/codex-appserver-poc/home_b codex app-server --listen ws://127.0.0.1:9003 &

# python client to each port:
#   :9002 initialize → codexHome=/tmp/codex-appserver-poc/home_a
#         thread/start → 019e1228-18b9-…
#   :9003 initialize → codexHome=/tmp/codex-appserver-poc/home_b
#         thread/start → 019e1228-1944-…  (different id)
```

**What this proves:**

- ✅ Per-thread subprocess model is feasible: independent ports,
  independent CODEX_HOMEs, independent thread ids, no sqlite lock
  contention.
- ✅ The "spawn per (workspace, thread)" recipe is sound.

### Wire format gotchas worth remembering for the Go impl

- **App-server v2 JSON-RPC envelope omits `jsonrpc:"2.0"`** — only `id`,
  `method`, `params`, `result|error`. Different from the MCP and
  exec-server protocols (which DO include `jsonrpc:"2.0"`). Our ws
  proxy is byte-level so this doesn't affect us, but tests that craft
  raw frames must match.
- **Server prints listen URL on stdout's first line** — same convention
  as `codex exec-server`. Parse from stdout to learn the random port.
- **Loopback default is no-auth** — unauthenticated connections accepted
  on `127.0.0.1`. For non-loopback we'd need `--ws-auth signed-bearer-token`.

### What the PoC did NOT cover

- `thread/resume` against a CODEX_HOME populated from S3 (phase-1 e2e
  must add).
- Idle shutdown / graceful flush + tar + S3 upload (phase-1 supervisor
  test).
- Concurrent connection rejection (phase-1 server test).
- Subprocess crash recovery (phase-1 supervisor test).
- A real LLM turn that completes successfully (PoC LLM 401'd; not
  blocking, the proxy layer is independently validated).

## Acceptance (phase 1)

A user can:

1. Start `codex --remote wss://AS/codex-app/...` on their laptop →
   gateway authenticates them and the TUI shows their threads.
2. Send a prompt that requires a shell command. The codex subprocess in
   the gateway pod sees `mcp__exe_xxx__shell` (per env-mcp PR #78),
   picks it, env-mcp child translates to exec-server frames, output
   flows back through the chain into the TUI.
3. Disconnect the TUI → reconnect on the same thread → `thread/resume`
   restores the conversation. If the subprocess was reaped during the
   gap, gateway pulls CODEX_HOME from S3, spawns a fresh subprocess,
   and the resume succeeds with no user-visible difference.
4. With two executors bound, the spawned codex sees both
   `mcp__exe_alpha__*` and `mcp__exe_beta__*` tool sets, picks per
   call (delegated to LLM behavior; same as MCP-rewrite spec).
5. Operator restarts the gateway pod. Active TUI sessions disconnect.
   Next reconnect spawns fresh subprocesses from S3, conversations
   resume cleanly.

A working end-to-end harness in docker-compose (pod + S3 fake +
codex-exec-gateway + a real `codex` binary spawned both as the
"laptop TUI" and as the "in-pod app-server subprocess") is the
acceptance gate before declaring phase 1 complete.

## Migration

The MCP-rewrite spec did not produce code for Subsystem 2 (only PR #78
for Subsystem 4). The plan files under
`docs/superpowers/plans/2026-05-05-codex-app-gateway-{foundations,runtime}.md`
are already marked OBSOLETE. Replace them with:

- `docs/superpowers/plans/2026-05-10-codex-app-gateway-subprocess.md`
  (new plan — to be written; will track this refinement task-by-task
  with TDD).

The existing `2026-05-05-codex-exec-gateway.md` plan still applies
unchanged (Subsystem 3 of the parent spec is unaffected).

PR #78 (env-mcp, merged or pending) carries forward unchanged.
