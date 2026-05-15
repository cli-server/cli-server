# codex-app-gateway: agentserver-issued bearer tokens — Design

**Status:** ready for plan
**Date:** 2026-05-15
**Supersedes:** parts of `2026-05-10-codex-app-gateway-subprocess.md` (supervisor key + inbound auth model). The subprocess lifecycle, S3 round-trip, ws frame proxy, and reaper sections of that spec are still in force.

## Goal

Replace codex-app-gateway's stand-alone HMAC bearer scheme with **agentserver-minted, DB-backed, user-issued tokens**, displayed via the agentserver web UI and pasted by the user into `codex --remote --remote-auth-token-env`. Simultaneously fix the supervisor's `(workspace, thread)` key — which forced gateway-side thread tracking via URL query — to a `(workspace)`-only key, so codex's native multi-thread protocol works unmodified.

## Non-goals

- Adding an OAuth flow on the codex-CLI side (codex's `--remote-auth-token-env` is the only supported handoff; we don't patch the codex fork).
- Per-thread CODEX_HOME isolation — superseded; codex app-server multiplexes threads inside one process.
- Multi-tenant per-user isolation within a workspace — workspace members share thread history (intentional collaboration).
- Token rotation / refresh — users revoke + regenerate (GitHub PAT model).

## Architecture

```
┌──────────────┐  1. POST /api/codex/tokens          ┌──────────────────┐
│ agentserver  │ ◄──────────────────────────────────│ Web UI (workspace│
│ session auth │                                    │ settings page)   │
│              │  2. resp { token: ast_… } once     │                  │
│              │ ──────────────────────────────────►│                  │
└──────┬───────┘                                    └──────────────────┘
       │ writes
       ▼
  ┌─────────────────────────┐
  │ Postgres                │
  │ codex_remote_tokens     │
  │ (id, ws_id, user_id,    │       3. user pastes ast_… into env, runs:
  │  bcrypt hash, exp, …)   │          codex --remote wss://.../codex-app/ws
  │                         │                          --remote-auth-token-env AGENTSERVER_TOKEN
  └─────────┬───────────────┘                                       │
            │                                                       │
            │ 5. POST /api/internal/codex/                          │
            │    tokens/verify                                      │
            │    { token } → { user_id, workspace_id }              ▼
            │                                                ┌──────────────┐
       ┌────┴───────────────┐  4. ws upgrade with Bearer    │ codex --remote
       │ agentserver        │ ◄─────────────────────────────│ TUI process │
       │ /api/internal/...  │                                └──────┬───────┘
       └────────────────────┘                                       │ proxies
                ▲                                                   │ frames
                │ Bearer = internal.apiSecret                       │
                │                                                   ▼
       ┌────────┴───────────┐                              ┌────────────────┐
       │ codex-app-gateway  │ ──6. EnsureSubprocess(ws_id) │ codex          │
       │ /codex-app/ws      │ ─────────────────────────────│ app-server     │
       │ (no URL params)    │                              │ subprocess per │
       └────────────────────┘                              │ workspace      │
                                                          └────────────────┘
```

## Data model

### New table `codex_remote_tokens`

```sql
CREATE TABLE codex_remote_tokens (
    id              TEXT PRIMARY KEY,            -- 8-char base36 (e.g. "a3k9f7zq")
    user_id         TEXT NOT NULL,
    workspace_id    TEXT NOT NULL,
    name            TEXT NOT NULL,
    token_hash      TEXT NOT NULL,               -- bcrypt(secret_part)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    last_used_at    TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ
);

CREATE INDEX idx_codex_tokens_user_workspace ON codex_remote_tokens(user_id, workspace_id);
CREATE INDEX idx_codex_tokens_workspace      ON codex_remote_tokens(workspace_id);
```

### Token format

```
ast_<id>_<secret>
   │     │
   │     └─ random 32 bytes → base36 (~165 bit entropy)
   └─ 8-char base36, == codex_remote_tokens.id
```

Example: `ast_a3k9f7zq_n2p4xj3w8q5r6t1...m`

The `id` is embedded so verification is one indexed `SELECT … WHERE id = $1` plus one `bcrypt.CompareHashAndPassword`. The raw secret never leaves the user's clipboard except as a bcrypt hash.

### Defaults

- TTL: 90 days default; mint API clamps `ttl_days ∈ [1, 365]`.
- Multiple tokens per (user, workspace) allowed (separate `name`).
- Revocation = soft (set `revoked_at`); rows kept for audit.
- `last_used_at` updated **best-effort, async** by the verify endpoint (goroutine, write failures logged not propagated).

## HTTP API

### User-facing (agentserver, session-authenticated)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/codex/tokens` | Mint. Body `{workspace_id, name, ttl_days?}`. 201 returns raw token **once**. |
| `GET`  | `/api/codex/tokens?workspace_id=ws_…` | List for that workspace. Excludes raw token + hash. `?include_revoked=true` adds revoked rows. |
| `DELETE` | `/api/codex/tokens/{id}` | Soft-revoke. Idempotent (re-DELETE → 204). Caller must be token owner OR workspace admin. |

ACL: caller must be a member of `workspace_id` (uses existing `db.GetWorkspaceMemberRole`). Non-members → 403.

### Service-to-service (agentserver internal, called by gateway)

`POST /api/internal/codex/tokens/verify`
- `Authorization: Bearer <internal.apiSecret>` (existing chart secret)
- Body `{ "token": "ast_…" }`
- 200: `{ "user_id": "usr_…", "workspace_id": "ws_…" }`
- 401: `{"error":"invalid_token"}` — same error for missing / bad-shape / wrong-secret / expired / revoked (no token enumeration)

Verify steps:
1. Parse `ast_<id>_<secret>`; shape error → 401
2. `SELECT user_id, workspace_id, token_hash, expires_at, revoked_at FROM codex_remote_tokens WHERE id = $1` → not found → 401
3. `bcrypt.CompareHashAndPassword(token_hash, secret)` → mismatch → 401
4. `expires_at < NOW()` OR `revoked_at IS NOT NULL` → 401
5. Async-update `last_used_at = NOW()` (goroutine; write errors logged warn, not propagated)
6. Return 200

### HTTP error matrix

| code | scenario |
|---|---|
| 401 | verify fail (any reason); missing/bad bearer |
| 403 | caller not workspace member (mint); not token owner / not workspace admin (delete) |
| 404 | token id not found (delete/list-by-id) |
| 422 | empty `name`, `ttl_days` out of `[1, 365]` |
| 500 | DB / bcrypt internal errors |

## codex-app-gateway changes

### Identity & Authenticator

```go
// internal/codexappgateway/auth/auth.go
type Identity struct {
    UserID      string
    WorkspaceID string
    // ThreadID removed — codex manages threads internally
}

type Authenticator interface {
    Verify(ctx context.Context, token string) (Identity, error)
}
```

Two implementations:
- `RemoteVerifier` (production) — POSTs to agentserver internal verify endpoint
- `HMACAuthenticator` (kept for tests + break-glass) — existing implementation; not used in chart-deployed pods

### `RemoteVerifier`

```go
type RemoteVerifier struct {
    baseURL    string  // CXG_AGENTSERVER_INTERNAL_URL
    bearer     string  // CXG_AGENTSERVER_INTERNAL_SECRET (== internal.apiSecret)
    httpClient *http.Client  // 5s timeout
}

func (v *RemoteVerifier) Verify(ctx context.Context, token string) (Identity, error) {
    // POST {baseURL}/api/internal/codex/tokens/verify
    // body: {"token": token}
    // 200 → decode {user_id, workspace_id} → Identity{UserID, WorkspaceID}
    // 401 / other → return ErrUnauthorized
}
```

### Supervisor key change

```go
// internal/codexappgateway/supervisor/supervisor.go
type Key struct {
    WorkspaceID string  // was {WorkspaceID, ThreadID}
}
```

Implications:
- 1 codex `app-server` subprocess per workspace, regardless of how many threads or users.
- codex's app-server is multi-client (`codex-rs/app-server/src/transport.rs` uses `connections: HashMap<ConnectionId, OutboundConnectionState>`); concurrent ws connects from multiple users in the same workspace each get a fresh ws→subprocess pair via the existing `RunProxy` pattern.
- Workspace members share thread history, sqlite, session JSONL — codex's native `thread/list` / `thread/start` / `thread/resume` RPCs handle picking.

### S3 layout

Old: `codex-app-gateway/<workspace_id>/<thread_id>.tar.gz`
New: `codex-app-gateway/<workspace_id>.tar.gz`

`codexhome.S3Backend` constructor signature drops `threadID`; key template adjusted; `Manager.NewTmpDir` keyed by workspace only.

### `handleCodexAppWS`

```go
func (s *Server) handleCodexAppWS(w http.ResponseWriter, r *http.Request) {
    tok, ok := auth.ExtractBearer(r)
    if !ok { http.Error(w, "missing Bearer", http.StatusUnauthorized); return }
    id, err := s.auth.Verify(r.Context(), tok)
    if err != nil { http.Error(w, "unauthorized", http.StatusUnauthorized); return }

    key := supervisor.Key{WorkspaceID: id.WorkspaceID}
    ctx := r.Context()
    handle, err := s.sup.EnsureSubprocess(ctx, key, func() (codexhome.ConfigInput, error) {
        return s.buildConfig(ctx, id.WorkspaceID)  // thread_id dropped here too
    })
    if err != nil { /* … */ }

    childWS, _, err := websocket.Dial(ctx, handle.WSURL, &websocket.DialOptions{
        CompressionMode: websocket.CompressionDisabled,
    })
    if err != nil { /* … */ }
    defer childWS.Close(websocket.StatusNormalClosure, "gateway closing")

    s.sup.Touch(key)
    if err := wsbridge.RunProxy(ctx, userWS, childWS, func() { s.sup.Touch(key) }); err != nil {
        s.logger.Info("proxy ended", "err", err, "key", key)
    }
}
```

`makeBuildConfig` signature changes correspondingly: drops `threadID` parameter and the per-spawn `turn_id` is generated per-spawn (not per-thread).

### Admin endpoint rename

`POST /admin/threads/restart` → `POST /admin/sessions/restart`
- Body `{ workspace_id }` — no thread_id
- Calls `sup.Shutdown(supervisor.Key{WorkspaceID})`

### URL form

`wss://<host>/codex-app/ws` — no query parameters, no path params. The bearer carries everything the gateway needs to know (user, workspace); codex carries everything else (thread, model, project) inside its native protocol.

## Web UI

New section in workspace settings page: **Codex Remote Access**

```
Codex Remote Access
─────────────────────
Use these tokens with `codex --remote wss://<host>/codex-app/ws --remote-auth-token-env <ENV_VAR>`.

[+ Generate new token]

┌──────────────────────────────────────────────────────────────────┐
│ Name        Created      Expires       Last used    Actions     │
├──────────────────────────────────────────────────────────────────┤
│ my mac      5d ago       in 85d        2h ago       [Revoke]    │
│ office vm   30d ago      in 60d        never        [Revoke]    │
└──────────────────────────────────────────────────────────────────┘
```

**Generate modal**
```
Name:    [my mac                    ]
TTL:     [90 days ▾]                  (1 / 7 / 30 / 90 / 180 / 365)
                                      [Cancel]  [Generate]
```

**Generated modal (one-time display)**
```
✓ Token generated. Copy it now — you won't see it again.

ast_a3k9f7zq_n2p4xj…8m                       [📋 Copy]

Quick start:
  export AGENTSERVER_TOKEN='ast_a3k9f7zq_n2p4xj…8m'
  codex --remote wss://<host>/codex-app/ws \
        --remote-auth-token-env AGENTSERVER_TOKEN
                                              [I've saved it]
```

Files: `web/src/pages/workspace/CodexTokens.tsx` + router entry + an axios/fetch hook in the existing API client. Estimated ~250 LOC TSX.

## Compatibility

- `auth.HMACAuthenticator` retained for unit tests + integration tests + break-glass token mint utility (`codex-app-gateway mint-token` subcommand stays valid as a dev tool).
- `CXG_INBOUND_HMAC_SECRET` env: required-when-set becomes optional; only `auth.RemoteVerifier` is used in chart-deployed pods.

## Configuration (chart)

`codexAppGateway` block adds:
- env `CXG_AGENTSERVER_INTERNAL_URL`: defaults to `http://{release}:{service.port}` rendered in template
- env `CXG_AGENTSERVER_INTERNAL_SECRET`: pulled from existing `{release}-secret` key `internal-api-secret` (already managed for cc-broker)

No new k8s Secret resources.

## Testing

### agentserver
- `internal/server/codex_tokens.go` + `_test.go`: handler unit tests for mint / list / revoke (uses existing test DB fixture pattern + workspace-membership ACL helper)
- `internal/server/codex_tokens_internal.go` + `_test.go`: verify endpoint with bcrypt round-trip
- DB migration test: `codex_remote_tokens` table creation + indexes

### codex-app-gateway
- `internal/codexappgateway/auth/remote_verifier.go` + `_test.go`: HTTP client with `httptest` fake agentserver covering success, 401, network error
- `internal/codexappgateway/server_test.go`: existing fixture switches to a `RemoteVerifier` backed by an `httptest` stub returning `{user_id, workspace_id}`; thread-id-from-url assertions deleted
- `internal/codexappgateway/supervisor/*_test.go`: `Key` literals updated to single-field; S3-key assertions match new `<workspace>.tar.gz` layout
- `internal/codexappgateway/codexhome/s3_test.go`: backend constructor + key template

### End-to-end smoke (post-deploy)
1. Mint token via web UI on a real workspace
2. `codex --remote wss://<host>/codex-app/ws --remote-auth-token-env $TOKEN` from a developer laptop
3. Verify subprocess spawned in gateway pod (`kubectl logs deploy/{release}-codex-app-gateway`)
4. `thread/list` returns historical threads; `thread/start` creates a new one
5. Revoke token via UI → next ws connect from same laptop returns 401

## Rollout

- chart bump: 0.49.x → **0.50.0** (minor; supervisor key change is technically breaking for any existing CODEX_HOME tarballs in S3)
- spec/plan supersede headers added to:
  - `docs/superpowers/specs/2026-05-10-codex-app-gateway-subprocess.md`
  - `docs/superpowers/plans/2026-05-11-codex-app-gateway-subprocess.md`
- Production data: existing `codex-app-gateway/<ws>/<thr>.tar.gz` keys (if any) deleted pre-cutover via `aws s3 rm --recursive` — confirmed acceptable since no real user traffic on the old per-thread layout yet.
- Pulumi stack `nj-prod`: `pulumi up` with chart 0.50.x; no Pulumi script changes (chart values stay the same).

## Open risks

1. **Multi-user concurrent writes to one workspace's sqlite** — codex app-server's per-connection state isolation is documented in source but not yet stress-tested by us. Mitigation: integration test with two simultaneous client connections; revisit if thread/turn collision turns into data corruption.
2. **`internal.apiSecret` reuse blast radius** — if this secret leaks, the attacker can mint+verify tokens AND call cc-broker / imbridge internal APIs. Acceptable today (single trust boundary in cluster) but a follow-up could split per-service secrets if we ever cross trust boundaries.
3. **Web-UI ACL drift** — workspace-admin check for revoking tokens not owned by the caller relies on `db.GetWorkspaceMemberRole`; if that role model changes, this code path needs to track it.

## Files (summary of net-new vs modified)

**New**
- `internal/server/codex_tokens.go` + `_test.go`
- `internal/server/codex_tokens_internal.go` + `_test.go`
- `internal/db/migrations/022_codex_remote_tokens.sql` (next free number after the existing 021_*.sql migrations)
- `internal/codexappgateway/auth/remote_verifier.go` + `_test.go`
- `web/src/pages/workspace/CodexTokens.tsx` + small hook
- `docs/superpowers/specs/2026-05-15-codex-app-gateway-oauth-bridge-design.md` (this doc)

**Modified**
- `internal/codexappgateway/auth/auth.go` (Identity + Authenticator interface)
- `internal/codexappgateway/supervisor/supervisor.go` (Key field)
- `internal/codexappgateway/supervisor/{spawn,reaper}.go` (Key literals + S3 backend signature)
- `internal/codexappgateway/codexhome/{codexhome,s3}.go` (drop threadID; key template)
- `internal/codexappgateway/server.go` (handleCodexAppWS / handleAdminRestart / makeBuildConfig)
- `internal/codexappgateway/config.go` (CXG_AGENTSERVER_INTERNAL_URL/SECRET)
- `cmd/codex-app-gateway/main.go` (no changes expected; verifier wiring inside NewServer)
- `deploy/helm/agentserver/{values.yaml,templates/codex-app-gateway.yaml}` (env additions)
- Existing `*_test.go` in `internal/codexappgateway/...`
- Header notes added to `docs/superpowers/specs/2026-05-10-codex-app-gateway-subprocess.md` and `docs/superpowers/plans/2026-05-11-codex-app-gateway-subprocess.md`

Estimated total new + modified LOC: ~1300 (Go + SQL + TSX + tests).
