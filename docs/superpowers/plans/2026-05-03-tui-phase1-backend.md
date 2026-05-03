# TUI Phase 1: Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the agentserver / cc-broker / executor-registry backend that an interactive TUI client (Phase 2) will consume. After Phase 1, all backend endpoints exist and pass integration tests; no client uses them yet.

**Architecture:** DB-first (two new migrations) → cc-broker permission gate + metadata threading + send_* TUI short-circuit + new transfer endpoints → agentserver new TUI endpoints + leak worker + SSE bridging. Fully backward-compatible with existing IM flow.

**Tech Stack:** Go (chi router, lib/pq, cc-broker SDK), PostgreSQL (separate DBs for agentserver / cc-broker / executor-registry).

**Spec:** [`docs/superpowers/specs/2026-05-03-agentserver-tui-design.md`](../specs/2026-05-03-agentserver-tui-design.md)

---

## File Structure (Phase 1 only)

| File | Created/Modified | Responsibility |
|---|---|---|
| `internal/db/migrations/021_tui_session_fields.sql` | Created | agentserver `agent_sessions` schema additions |
| `internal/executorregistry/migrations/002_executor_owner_sharing.sql` | Created | executor-registry `executors` owner_user_id + shared_to_workspace |
| `internal/db/agent_sessions.go` | Modified | AgentSession struct + new queries (CAS, attach, list-by-owner) |
| `internal/executorregistry/store.go` | Modified | ExecutorInfo struct + register accepts owner_user_id |
| `internal/executorregistry/handler_register.go` | Modified | Persist owner_user_id from register request |
| `internal/executorregistry/models.go` | Modified | Register request schema |
| `internal/ccbroker/tools/context.go` | Modified | New fields (Gate, PermissionMode, CreatorUserID, PreferredExecutorID, AgentserverInternalURL) |
| `internal/ccbroker/tools/permission.go` | Created | Gate state machine: Check / Resolve / CancelTurn / sticky rules |
| `internal/ccbroker/tools/prompt.go` | Created | BuildSystemPrompt with preferred executor + channel hint |
| `internal/ccbroker/tools/executor.go` | Modified | Wrap each remote_* handler with Gate.Check |
| `internal/ccbroker/tools/im.go` | Modified | send_* short-circuit when IMChannelID is empty |
| `internal/ccbroker/handler_turns.go` | Modified | Parse metadata; turn_kind=compaction; defer turn-finished callback |
| `internal/ccbroker/runner/options.go` | Modified | metadata → SDK options (model, system prompt) |
| `internal/ccbroker/handler_tui_routes.go` | Created | New endpoints: cancel, decide, compact, get-active-turn |
| `internal/ccbroker/server.go` | Modified | Register new routes |
| `internal/server/handler_tui_inbound.go` | Created | POST /api/workspaces/{wid}/tui/inbound (with CAS) |
| `internal/server/handler_tui_session.go` | Created | POST /api/agent-sessions, /attach, GET /api/agent-sessions |
| `internal/server/handler_tui_events.go` | Created | GET /api/agent-sessions/{sid}/events (SSE) |
| `internal/server/handler_tui_control.go` | Created | POST /control with command dispatch |
| `internal/server/handler_tui_proxy.go` | Created | cancel + decision + executor status proxy handlers |
| `internal/server/handler_tui_internal.go` | Created | POST /internal/sessions/{sid}/turn-finished |
| `internal/server/leak_worker.go` | Created | Background worker: stale active_turn_id + responder TTL |
| `internal/server/server.go` | Modified | Register new routes; start leak worker |

---

## Task Sequencing

DB migrations first (Tasks 1-2), then data layer extensions (3-5), then cc-broker layers from inside-out (Gate first 6, then wrapping/threading 7-11, then new HTTP endpoints 12-13), then agentserver inbound + session lifecycle 14-16, then SSE bridging 17-18, then proxy/control endpoints 19-21, then leak worker 22, then full wiring 23.

---

## Task 1: agentserver DB migration — TUI session fields

**Files:**
- Create: `internal/db/migrations/021_tui_session_fields.sql`

- [ ] **Step 1: Write the migration SQL**

```sql
-- 021_tui_session_fields.sql
-- Adds session fields needed by TUI client (channel routing, model preference,
-- permission mode, preferred executor, responder claim, active turn CAS).

ALTER TABLE agent_sessions
  ADD COLUMN IF NOT EXISTS channel_type           TEXT NOT NULL DEFAULT 'im',
  ADD COLUMN IF NOT EXISTS creator_user_id        TEXT,
  ADD COLUMN IF NOT EXISTS preferred_model        TEXT,
  ADD COLUMN IF NOT EXISTS permission_mode        TEXT NOT NULL DEFAULT 'bypass',
  ADD COLUMN IF NOT EXISTS preferred_executor_id  TEXT,
  ADD COLUMN IF NOT EXISTS permission_responder   TEXT,
  ADD COLUMN IF NOT EXISTS responder_attached_at  TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS active_turn_id         TEXT;

-- Restrictive design: legacy IM-flow rows keep creator_user_id NULL. The
-- TUI inbound path (the only consumer of this field) populates it from the
-- authenticated user_id. See spec §4.11 for the full rationale and the
-- mirrored executor.owner_user_id handling.

CREATE INDEX IF NOT EXISTS idx_agent_sessions_channel_external
  ON agent_sessions (workspace_id, channel_type, external_id);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_responder
  ON agent_sessions (permission_responder) WHERE permission_responder IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_sessions_active_turn
  ON agent_sessions (active_turn_id) WHERE active_turn_id IS NOT NULL;
```

- [ ] **Step 2: Apply migration to local dev DB and verify**

Run: `psql $DATABASE_URL -f internal/db/migrations/021_tui_session_fields.sql`
Then: `psql $DATABASE_URL -c "\\d agent_sessions"`
Expected: 8 new columns visible; existing rows have `channel_type='im'`, `permission_mode='bypass'`, `creator_user_id` remains NULL (intentionally — see comment in migration).

- [ ] **Step 3: Verify migration is loaded by db.New** — `internal/db/db.go` uses `embed.FS` over `migrations/*.sql`; new file is auto-discovered.

Run: `go test ./internal/db/...`
Expected: existing tests still pass; no schema-related errors.

- [ ] **Step 4: Commit**

```bash
git add internal/db/migrations/021_tui_session_fields.sql
git commit -m "feat(db): add TUI session fields to agent_sessions"
```

---

## Task 2: executor-registry DB migration — owner & sharing fields

**Files:**
- Create: `internal/executorregistry/migrations/002_executor_owner_sharing.sql`

- [ ] **Step 1: Write the migration SQL**

```sql
-- 002_executor_owner_sharing.sql
-- Adds owner_user_id (for cross-user invocation hard-deny) and
-- shared_to_workspace (v1.x dual-consent opt-in; v1 always FALSE).

ALTER TABLE executors
  ADD COLUMN IF NOT EXISTS owner_user_id          TEXT,
  ADD COLUMN IF NOT EXISTS shared_to_workspace    BOOLEAN NOT NULL DEFAULT FALSE;

-- Restrictive cross-user policy. Legacy executors keep owner_user_id NULL.
-- GetExecutor (Task 4) projects NULL → 'unknown' via COALESCE; gate.Check
-- (Task 6) compares 'unknown' against the session creator's real user id,
-- which never matches → cross_user_denied. Legacy executors must be
-- re-registered to be invocable. See spec §4.11.

CREATE INDEX IF NOT EXISTS idx_executors_owner ON executors(owner_user_id);
```

- [ ] **Step 2: Apply migration and verify**

Run: `psql $EXREG_DATABASE_URL -f internal/executorregistry/migrations/002_executor_owner_sharing.sql`
Then: `psql $EXREG_DATABASE_URL -c "\\d executors"`
Expected: `owner_user_id` (TEXT) + `shared_to_workspace` (BOOLEAN, default FALSE).

- [ ] **Step 3: Run existing executor-registry tests**

Run: `go test ./internal/executorregistry/...`
Expected: pass.

- [ ] **Step 4: Commit**

```bash
git add internal/executorregistry/migrations/002_executor_owner_sharing.sql
git commit -m "feat(exreg): add owner_user_id and shared_to_workspace to executors"
```

---

## Task 3: agentserver — extend AgentSession struct & add CAS / attach / list-by-channel queries

**Files:**
- Modify: `internal/db/agent_sessions.go`
- Test: `internal/db/agent_sessions_test.go` (create if missing)

- [ ] **Step 1: Write the failing test for new fields & CAS**

```go
// internal/db/agent_sessions_test.go
package db

import (
    "context"
    "testing"
    "time"
)

func TestAgentSessionTUIFields(t *testing.T) {
    d := newTestDB(t)
    ctx := context.Background()

    // Create a TUI-style session.
    sid := "cse_test1"
    err := d.CreateAgentSessionTUI(ctx, CreateTUISessionParams{
        ID:                  sid,
        WorkspaceID:         "ws_test",
        ExternalID:          "tui:exe_a:1730000000",
        Title:               "TUI session",
        CreatorUserID:       "u_alice",
        PermissionMode:      "ask",
        PreferredExecutorID: "exe_a",
    })
    if err != nil {
        t.Fatalf("create: %v", err)
    }

    s, err := d.GetAgentSession(sid)
    if err != nil || s == nil {
        t.Fatalf("get: %v %v", s, err)
    }
    if s.ChannelType != "tui" {
        t.Errorf("channel_type=%q, want tui", s.ChannelType)
    }
    if s.PermissionMode != "ask" {
        t.Errorf("permission_mode=%q, want ask", s.PermissionMode)
    }
    if s.PreferredExecutorID == nil || *s.PreferredExecutorID != "exe_a" {
        t.Errorf("preferred_executor_id=%v, want exe_a", s.PreferredExecutorID)
    }
}

func TestActiveTurnCAS(t *testing.T) {
    d := newTestDB(t)
    ctx := context.Background()
    sid := "cse_cas"
    if err := d.CreateAgentSessionTUI(ctx, CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1", CreatorUserID: "u",
    }); err != nil {
        t.Fatal(err)
    }

    ok, err := d.ClaimActiveTurn(ctx, sid, "trn_a")
    if err != nil || !ok {
        t.Fatalf("first claim should succeed: ok=%v err=%v", ok, err)
    }
    ok, _ = d.ClaimActiveTurn(ctx, sid, "trn_b")
    if ok {
        t.Errorf("second claim should fail (turn in progress)")
    }
    cur, _ := d.GetActiveTurn(ctx, sid)
    if cur != "trn_a" {
        t.Errorf("active_turn_id=%q want trn_a", cur)
    }
    if err := d.ClearActiveTurn(ctx, sid, "trn_a"); err != nil {
        t.Fatalf("clear: %v", err)
    }
    cur2, _ := d.GetActiveTurn(ctx, sid)
    if cur2 != "" {
        t.Errorf("after clear, active_turn_id=%q want empty", cur2)
    }
}

func TestAttachResponder(t *testing.T) {
    d := newTestDB(t)
    ctx := context.Background()
    sid := "cse_att"
    _ = d.CreateAgentSessionTUI(ctx, CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws", ExternalID: "tui:e:1", CreatorUserID: "u",
    })

    prev, err := d.AttachResponder(ctx, sid, "exe_laptop", true /*becomePreferred*/)
    if err != nil {
        t.Fatalf("first attach: %v", err)
    }
    if prev.PreviousResponder != "" || prev.PreviousPreferred != "" {
        t.Errorf("first attach should have no previous: %+v", prev)
    }

    prev2, _ := d.AttachResponder(ctx, sid, "exe_desktop", true)
    if prev2.PreviousResponder != "exe_laptop" {
        t.Errorf("second attach previous_responder=%q want exe_laptop", prev2.PreviousResponder)
    }
    if prev2.PreviousPreferred != "exe_laptop" {
        t.Errorf("second attach previous_preferred=%q want exe_laptop", prev2.PreviousPreferred)
    }
}

func TestListSessionsByChannel(t *testing.T) {
    d := newTestDB(t)
    ctx := context.Background()
    _ = d.CreateAgentSessionTUI(ctx, CreateTUISessionParams{
        ID: "cse_1", WorkspaceID: "ws", ExternalID: "tui:exe_a:100", CreatorUserID: "u",
    })
    time.Sleep(10 * time.Millisecond)
    _ = d.CreateAgentSessionTUI(ctx, CreateTUISessionParams{
        ID: "cse_2", WorkspaceID: "ws", ExternalID: "tui:exe_a:200", CreatorUserID: "u",
    })

    list, err := d.ListSessionsByChannel(ctx, "ws", "tui", "exe_a", 10)
    if err != nil {
        t.Fatal(err)
    }
    if len(list) != 2 {
        t.Fatalf("got %d sessions, want 2", len(list))
    }
    if list[0].ID != "cse_2" {
        t.Errorf("first should be most recent (cse_2), got %q", list[0].ID)
    }
}
```

`newTestDB(t)` is the existing test helper if present; if not, add one in a new `internal/db/testdb_test.go` that opens `os.Getenv("TEST_DATABASE_URL")` and runs migrations once per package via `sync.Once`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run "TestAgentSessionTUIFields|TestActiveTurnCAS|TestAttachResponder|TestListSessionsByChannel" -v`
Expected: FAIL — methods `CreateAgentSessionTUI`, `ClaimActiveTurn`, `GetActiveTurn`, `ClearActiveTurn`, `AttachResponder`, `ListSessionsByChannel` not defined; new fields not on `AgentSession`.

- [ ] **Step 3: Extend AgentSession struct**

In `internal/db/agent_sessions.go` modify the struct (keep existing fields, add new ones; preserve order so existing scans still match):

```go
type AgentSession struct {
    // ... existing fields ...
    ChannelType         string     `json:"channel_type"`
    CreatorUserID       string     `json:"creator_user_id"`
    PreferredModel      *string    `json:"preferred_model"`
    PermissionMode      string     `json:"permission_mode"`
    PreferredExecutorID *string    `json:"preferred_executor_id"`
    PermissionResponder *string    `json:"permission_responder"`
    ResponderAttachedAt *time.Time `json:"responder_attached_at"`
    ActiveTurnID        *string    `json:"active_turn_id"`
}
```

Then update existing `GetAgentSession` SELECT to include the new columns and Scan into pointers.

- [ ] **Step 4: Add CreateAgentSessionTUI**

```go
type CreateTUISessionParams struct {
    ID                  string
    WorkspaceID         string
    ExternalID          string
    Title               string
    CreatorUserID       string
    PermissionMode      string  // "ask" | "bypass"
    PreferredExecutorID string  // optional
    PreferredModel      string  // optional
}

func (db *DB) CreateAgentSessionTUI(ctx context.Context, p CreateTUISessionParams) error {
    if p.PermissionMode == "" {
        p.PermissionMode = "ask"
    }
    _, err := db.ExecContext(ctx, `
        INSERT INTO agent_sessions
          (id, sandbox_id, workspace_id, title, status, source, channel_type,
           external_id, creator_user_id, permission_mode,
           preferred_executor_id, preferred_model, tags)
        VALUES ($1, NULL, $2, $3, 'active', 'tui', 'tui',
                $4, $5, $6,
                NULLIF($7, ''), NULLIF($8, ''), '{}')`,
        p.ID, p.WorkspaceID, p.Title,
        p.ExternalID, p.CreatorUserID, p.PermissionMode,
        p.PreferredExecutorID, p.PreferredModel,
    )
    return err
}
```

- [ ] **Step 5: Add ClaimActiveTurn / GetActiveTurn / ClearActiveTurn**

```go
func (db *DB) ClaimActiveTurn(ctx context.Context, sessionID, turnID string) (bool, error) {
    res, err := db.ExecContext(ctx, `
        UPDATE agent_sessions SET active_turn_id = $1, updated_at = NOW()
         WHERE id = $2 AND active_turn_id IS NULL`, turnID, sessionID)
    if err != nil {
        return false, err
    }
    n, _ := res.RowsAffected()
    return n == 1, nil
}

func (db *DB) GetActiveTurn(ctx context.Context, sessionID string) (string, error) {
    var s sql.NullString
    err := db.QueryRowContext(ctx,
        `SELECT active_turn_id FROM agent_sessions WHERE id = $1`, sessionID).Scan(&s)
    if err != nil {
        return "", err
    }
    return s.String, nil
}

// ClearActiveTurn only clears if the current value matches expectedTurnID.
// This guards against late callbacks from a stale turn clobbering a fresh one.
func (db *DB) ClearActiveTurn(ctx context.Context, sessionID, expectedTurnID string) error {
    _, err := db.ExecContext(ctx, `
        UPDATE agent_sessions SET active_turn_id = NULL, updated_at = NOW()
         WHERE id = $1 AND active_turn_id = $2`, sessionID, expectedTurnID)
    return err
}
```

- [ ] **Step 6: Add AttachResponder**

```go
type AttachResult struct {
    PreviousResponder string
    PreviousPreferred string
}

func (db *DB) AttachResponder(ctx context.Context, sessionID, executorID string, becomePreferred bool) (AttachResult, error) {
    var prev AttachResult
    var prevResp, prevPref sql.NullString
    err := db.QueryRowContext(ctx, `
        SELECT COALESCE(permission_responder, ''), COALESCE(preferred_executor_id, '')
          FROM agent_sessions WHERE id = $1`, sessionID).Scan(&prevResp, &prevPref)
    if err != nil {
        return prev, err
    }
    prev.PreviousResponder = prevResp.String
    prev.PreviousPreferred = prevPref.String

    if becomePreferred {
        _, err = db.ExecContext(ctx, `
            UPDATE agent_sessions
               SET permission_responder = $1,
                   preferred_executor_id = $1,
                   responder_attached_at = NOW(),
                   updated_at = NOW()
             WHERE id = $2`, executorID, sessionID)
    } else {
        _, err = db.ExecContext(ctx, `
            UPDATE agent_sessions
               SET permission_responder = $1,
                   responder_attached_at = NOW(),
                   updated_at = NOW()
             WHERE id = $2`, executorID, sessionID)
    }
    return prev, err
}

// ClearResponder unsets permission_responder for a session. Used by leak worker
// when the SSE connection has been gone past the TTL.
func (db *DB) ClearResponder(ctx context.Context, sessionID string) error {
    _, err := db.ExecContext(ctx, `
        UPDATE agent_sessions
           SET permission_responder = NULL,
               responder_attached_at = NULL,
               updated_at = NOW()
         WHERE id = $1`, sessionID)
    return err
}
```

- [ ] **Step 7: Add ListSessionsByChannel**

```go
type SessionListItem struct {
    ID                  string
    ExternalID          string
    Title               string
    LastActivityAt      time.Time
    PermissionResponder *string
}

func (db *DB) ListSessionsByChannel(ctx context.Context, workspaceID, channelType, executorID string, limit int) ([]SessionListItem, error) {
    if limit <= 0 || limit > 100 {
        limit = 20
    }
    rows, err := db.QueryContext(ctx, `
        SELECT id, COALESCE(external_id, ''), title, updated_at, permission_responder
          FROM agent_sessions
         WHERE workspace_id = $1
           AND channel_type = $2
           AND external_id LIKE $3
           AND archived_at IS NULL
         ORDER BY updated_at DESC
         LIMIT $4`,
        workspaceID, channelType,
        fmt.Sprintf("%s:%s:%%", channelType, executorID),
        limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var out []SessionListItem
    for rows.Next() {
        var it SessionListItem
        if err := rows.Scan(&it.ID, &it.ExternalID, &it.Title, &it.LastActivityAt, &it.PermissionResponder); err != nil {
            return nil, err
        }
        out = append(out, it)
    }
    return out, rows.Err()
}

// ListStaleResponders returns session IDs whose responder_attached_at is
// older than cutoff. Used by leak worker.
func (db *DB) ListStaleResponders(ctx context.Context, cutoff time.Time) ([]string, error) {
    rows, err := db.QueryContext(ctx, `
        SELECT id FROM agent_sessions
         WHERE permission_responder IS NOT NULL
           AND responder_attached_at < $1`, cutoff)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var ids []string
    for rows.Next() {
        var s string
        if err := rows.Scan(&s); err != nil {
            return nil, err
        }
        ids = append(ids, s)
    }
    return ids, rows.Err()
}

// ListStaleActiveTurns returns (session_id, active_turn_id) pairs older than cutoff.
func (db *DB) ListStaleActiveTurns(ctx context.Context, cutoff time.Time) ([]struct{ SessionID, TurnID string }, error) {
    rows, err := db.QueryContext(ctx, `
        SELECT id, active_turn_id FROM agent_sessions
         WHERE active_turn_id IS NOT NULL
           AND updated_at < $1`, cutoff)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var out []struct{ SessionID, TurnID string }
    for rows.Next() {
        var s struct{ SessionID, TurnID string }
        if err := rows.Scan(&s.SessionID, &s.TurnID); err != nil {
            return nil, err
        }
        out = append(out, s)
    }
    return out, rows.Err()
}
```

- [ ] **Step 8: Run test to verify all pass**

Run: `go test ./internal/db/ -run "TestAgentSession|TestActiveTurn|TestAttachResponder|TestListSessions" -v`
Expected: all PASS.

- [ ] **Step 9: Run full db package test to ensure no regression**

Run: `go test ./internal/db/...`
Expected: all PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/db/agent_sessions.go internal/db/agent_sessions_test.go
git commit -m "feat(db): TUI session struct fields, CAS, responder, list-by-channel queries"
```

---

## Task 4: executor-registry — extend ExecutorInfo & accept owner_user_id at register

**Files:**
- Modify: `internal/executorregistry/store.go`
- Modify: `internal/executorregistry/handler_register.go`
- Modify: `internal/executorregistry/models.go`
- Test: `internal/executorregistry/integration_test.go`

- [ ] **Step 1: Write failing test for owner_user_id round-trip**

```go
// In internal/executorregistry/integration_test.go, append:
func TestRegisterPersistsOwnerUserID(t *testing.T) {
    srv := newTestServer(t)
    body := map[string]string{
        "name":          "test-exec",
        "workspace_id":  "ws_test",
        "owner_user_id": "u_alice",
    }
    rr := doRequest(t, srv, http.MethodPost, "/api/executors/register", body, "")
    if rr.Code != http.StatusCreated {
        t.Fatalf("status %d", rr.Code)
    }
    var reg struct {
        ExecutorID string `json:"executor_id"`
    }
    json.Unmarshal(rr.Body.Bytes(), &reg)

    info, err := srv.store.GetExecutor(context.Background(), reg.ExecutorID)
    if err != nil || info == nil {
        t.Fatalf("GetExecutor: %v %v", info, err)
    }
    if info.OwnerUserID != "u_alice" {
        t.Errorf("owner_user_id=%q want u_alice", info.OwnerUserID)
    }
    if info.SharedToWorkspace {
        t.Errorf("shared_to_workspace=true want false (default)")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/executorregistry/ -run TestRegisterPersistsOwnerUserID -v`
Expected: FAIL — `OwnerUserID` / `SharedToWorkspace` not on `ExecutorInfo`; register doesn't persist.

- [ ] **Step 3: Extend ExecutorInfo struct**

In `internal/executorregistry/store.go` add fields to `ExecutorInfo`:

```go
type ExecutorInfo struct {
    // ... existing fields ...
    OwnerUserID       string `json:"owner_user_id"`
    SharedToWorkspace bool   `json:"shared_to_workspace"`
}
```

Update `GetExecutor` and `ListExecutors` SELECT statements to include the new columns and Scan them.

For `GetExecutor`:
```go
err := s.QueryRowContext(ctx, `
    SELECT e.id, e.workspace_id, e.name, e.type, e.status, e.created_at, e.updated_at,
           COALESCE(e.owner_user_id, 'unknown'), COALESCE(e.shared_to_workspace, FALSE),
           ec.tools, ec.environment, ec.resources, ec.description, ec.working_dir, ec.probed_at,
           eh.last_seen, eh.system_info
      FROM executors e
      LEFT JOIN executor_capabilities ec ON ec.executor_id = e.id
      LEFT JOIN executor_heartbeats eh ON eh.executor_id = e.id
     WHERE e.id = $1`, id).Scan(
    &info.ID, &info.WorkspaceID, &info.Name, &info.Type, &info.Status,
    &info.CreatedAt, &info.UpdatedAt,
    &info.OwnerUserID, &info.SharedToWorkspace,
    /* ... rest of fields ... */)
```

Same shape for `ListExecutors`.

- [ ] **Step 4: Add SetOwnerUserID to register insert**

In `internal/executorregistry/store.go` modify `RegisterExecutor` to accept and persist owner_user_id:

```go
func (s *Store) RegisterExecutor(ctx context.Context, p RegisterParams) (*RegisterResult, error) {
    // ... existing code ...
    _, err := s.ExecContext(ctx, `
        INSERT INTO executors (id, workspace_id, name, type, status,
                               tunnel_token_hash, registry_token_hash,
                               owner_user_id, shared_to_workspace,
                               created_at, updated_at)
        VALUES ($1, $2, $3, $4, 'online', $5, $6, $7, FALSE, NOW(), NOW())`,
        id, p.WorkspaceID, p.Name, p.Type,
        tunnelHash, registryHash,
        p.OwnerUserID,
    )
    // ...
}
```

Add `OwnerUserID string` to `RegisterParams`.

- [ ] **Step 5: Update register handler to read owner_user_id**

In `internal/executorregistry/handler_register.go`:

```go
type registerRequest struct {
    Name        string `json:"name"`
    WorkspaceID string `json:"workspace_id"`
    OwnerUserID string `json:"owner_user_id"`  // new; "unknown" if absent (back-compat)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
    var req registerRequest
    json.NewDecoder(r.Body).Decode(&req)
    // existing validation
    if req.OwnerUserID == "" {
        req.OwnerUserID = "unknown"  // back-compat for old agent binaries
    }
    res, err := s.store.RegisterExecutor(r.Context(), RegisterParams{
        Name:        req.Name,
        WorkspaceID: req.WorkspaceID,
        Type:        "local_agent",
        OwnerUserID: req.OwnerUserID,
    })
    // ... existing response writing ...
}
```

- [ ] **Step 6: Run new test to verify pass**

Run: `go test ./internal/executorregistry/ -run TestRegisterPersistsOwnerUserID -v`
Expected: PASS.

- [ ] **Step 7: Run full executor-registry test suite**

Run: `go test ./internal/executorregistry/...`
Expected: all PASS (existing tests still work because OwnerUserID defaults to "unknown").

- [ ] **Step 8: Commit**

```bash
git add internal/executorregistry/store.go internal/executorregistry/handler_register.go internal/executorregistry/models.go internal/executorregistry/integration_test.go
git commit -m "feat(exreg): persist owner_user_id at registration; backward compatible"
```

---

## Task 5: cc-broker — extend tools.Context with new fields

**Files:**
- Modify: `internal/ccbroker/tools/context.go`

- [ ] **Step 1: Extend the struct**

```go
package tools

import (
    "net/http"
    "github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

type Context struct {
    // existing
    SessionID           string
    WorkspaceID         string
    IMChannelID         string
    IMUserID            string
    ExecutorRegistryURL string
    AgentserverURL      string
    IMBridgeURL         string
    InternalAPISecret   string
    Workspace           *workspace.Workspace
    Viking              *workspace.VikingClient
    HTTP                *http.Client

    // new (TUI / permission gate)
    ChannelType            string  // "im" | "tui"
    CreatorUserID          string  // for cross-user check
    PermissionMode         string  // "ask" | "bypass"
    PreferredExecutorID    string  // optional; injected into system prompt
    Gate                   *Gate   // reference to per-broker singleton; tools call Gate.Check
    AgentserverInternalURL string  // for turn-finished callback (read from cc-broker config)
    CurrentTurnID          string  // set per turn by handler_turns
}
```

- [ ] **Step 2: Verify it still compiles (no behaviour change yet)**

Run: `go build ./internal/ccbroker/...`
Expected: success.

- [ ] **Step 3: Commit**

```bash
git add internal/ccbroker/tools/context.go
git commit -m "feat(ccbroker): extend tools.Context with TUI / permission gate fields"
```

---

## Task 6: cc-broker — Permission Gate (core)

**Files:**
- Create: `internal/ccbroker/tools/permission.go`
- Test: `internal/ccbroker/tools/permission_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/ccbroker/tools/permission_test.go
package tools

import (
    "context"
    "encoding/json"
    "fmt"
    "sync"
    "testing"
    "time"
)

func captureNotifier() (*Gate, *[]Event) {
    var mu sync.Mutex
    events := []Event{}
    g := NewGate(func(sid string, e Event) {
        mu.Lock()
        events = append(events, e)
        mu.Unlock()
    })
    return g, &events
}

func TestGate_BypassMode_AllowsImmediately(t *testing.T) {
    g, _ := captureNotifier()
    err := g.Check(context.Background(), CheckRequest{
        SessionID:                "s1",
        TurnID:                   "t1",
        Tool:                     "remote_bash",
        ExecutorID:               "exe_a",
        Args:                     json.RawMessage(`{"command":"ls"}`),
        PermissionMode:           "bypass",
        SessionCreatorUserID:     "u",
        ExecutorOwnerUserID:      "u",
        Timeout:                  100 * time.Millisecond,
    })
    if err != nil {
        t.Errorf("bypass should allow: %v", err)
    }
}

func TestGate_CrossUser_DeniesImmediately(t *testing.T) {
    g, _ := captureNotifier()
    err := g.Check(context.Background(), CheckRequest{
        SessionID:                "s1",
        TurnID:                   "t1",
        Tool:                     "remote_bash",
        ExecutorID:               "exe_a",
        Args:                     json.RawMessage(`{"command":"ls"}`),
        PermissionMode:           "ask",
        SessionCreatorUserID:     "u_alice",
        ExecutorOwnerUserID:      "u_bob",
        ExecutorSharedToWorkspace: false,
        Timeout:                  100 * time.Millisecond,
    })
    if err != ErrCrossUserDenied {
        t.Errorf("err=%v want ErrCrossUserDenied", err)
    }
}

func TestGate_AskMode_BlocksUntilResolve(t *testing.T) {
    g, events := captureNotifier()
    done := make(chan error, 1)
    go func() {
        done <- g.Check(context.Background(), CheckRequest{
            SessionID:            "s1",
            TurnID:               "t1",
            Tool:                 "remote_bash",
            ExecutorID:           "exe_a",
            Args:                 json.RawMessage(`{"command":"ls"}`),
            PermissionMode:       "ask",
            SessionCreatorUserID: "u",
            ExecutorOwnerUserID:  "u",
            Timeout:              5 * time.Second,
        })
    }()
    // wait for permission_request event to be emitted
    var pid string
    deadline := time.Now().Add(time.Second)
    for time.Now().Before(deadline) {
        if len(*events) > 0 {
            pid = (*events)[0].PermissionID
            break
        }
        time.Sleep(5 * time.Millisecond)
    }
    if pid == "" {
        t.Fatal("no permission_request emitted")
    }
    if err := g.Resolve(pid, Decision{Verdict: "allow", Scope: "once"}); err != nil {
        t.Fatal(err)
    }
    select {
    case err := <-done:
        if err != nil {
            t.Errorf("Check should allow after resolve, got %v", err)
        }
    case <-time.After(time.Second):
        t.Fatal("Check did not return after Resolve")
    }
    // permission_resolved should also be emitted
    var sawResolved bool
    for _, e := range *events {
        if e.Type == "permission_resolved" {
            sawResolved = true
        }
    }
    if !sawResolved {
        t.Errorf("no permission_resolved event")
    }
}

func TestGate_AskMode_TimeoutDenies(t *testing.T) {
    g, _ := captureNotifier()
    err := g.Check(context.Background(), CheckRequest{
        SessionID:            "s1",
        TurnID:               "t1",
        Tool:                 "remote_bash",
        ExecutorID:           "exe_a",
        Args:                 json.RawMessage(`{"command":"ls"}`),
        PermissionMode:       "ask",
        SessionCreatorUserID: "u",
        ExecutorOwnerUserID:  "u",
        Timeout:              50 * time.Millisecond,
    })
    if err != ErrPermissionDenied {
        t.Errorf("err=%v want ErrPermissionDenied (timeout)", err)
    }
}

func TestGate_StickyAlways_HitsWithoutEmit(t *testing.T) {
    g, events := captureNotifier()
    base := CheckRequest{
        SessionID:            "s1",
        TurnID:               "t1",
        Tool:                 "remote_bash",
        ExecutorID:           "exe_a",
        Args:                 json.RawMessage(`{"command":"git status"}`),
        PermissionMode:       "ask",
        SessionCreatorUserID: "u",
        ExecutorOwnerUserID:  "u",
        Timeout:              5 * time.Second,
    }
    // First call: resolve with always
    done := make(chan error, 1)
    go func() { done <- g.Check(context.Background(), base) }()
    var pid string
    for i := 0; i < 200 && pid == ""; i++ {
        if len(*events) > 0 {
            pid = (*events)[0].PermissionID
        }
        time.Sleep(5 * time.Millisecond)
    }
    g.Resolve(pid, Decision{Verdict: "allow", Scope: "always"})
    if err := <-done; err != nil {
        t.Fatalf("first call: %v", err)
    }
    eventsBefore := len(*events)
    // Second call with same head ("git" first token): sticky hit
    base2 := base
    base2.Args = json.RawMessage(`{"command":"git status -s"}`)
    if err := g.Check(context.Background(), base2); err != nil {
        t.Errorf("sticky should allow: %v", err)
    }
    // Should emit permission_resolved (scope=sticky) but NOT permission_request
    eventsAfter := len(*events)
    if eventsAfter <= eventsBefore {
        t.Errorf("expected at least one new event (resolved/sticky)")
    }
    for i := eventsBefore; i < eventsAfter; i++ {
        if (*events)[i].Type == "permission_request" {
            t.Errorf("sticky path should NOT emit permission_request")
        }
    }
    // Third call with DIFFERENT head: should ask again (no auto-allow)
    base3 := base
    base3.Args = json.RawMessage(`{"command":"docker ps"}`)
    base3.Timeout = 50 * time.Millisecond
    err := g.Check(context.Background(), base3)
    if err != ErrPermissionDenied {
        t.Errorf("different head should re-ask (and time out → deny), got %v", err)
    }
}

func TestGate_CancelTurn_ResolvesAllPendingOfThatTurn(t *testing.T) {
    g, _ := captureNotifier()
    var wg sync.WaitGroup
    errs := make([]error, 3)
    for i := 0; i < 3; i++ {
        wg.Add(1)
        i := i
        go func() {
            defer wg.Done()
            errs[i] = g.Check(context.Background(), CheckRequest{
                SessionID:            "s1",
                TurnID:               "t_cancel",
                Tool:                 "remote_bash",
                ExecutorID:           "exe_a",
                Args:                 json.RawMessage(fmt.Sprintf(`{"command":"cmd%d"}`, i)),
                PermissionMode:       "ask",
                SessionCreatorUserID: "u",
                ExecutorOwnerUserID:  "u",
                Timeout:              5 * time.Second,
            })
        }()
    }
    time.Sleep(50 * time.Millisecond)
    g.CancelTurn("t_cancel")
    wg.Wait()
    for i, err := range errs {
        if err != ErrPermissionDenied {
            t.Errorf("call %d: err=%v want ErrPermissionDenied (cancelled)", i, err)
        }
    }
}

func TestMakeRuleKey_BashHeadIsTwoTokens(t *testing.T) {
    cases := []struct {
        cmd  string
        want string
    }{
        {"git status", "remote_bash|exe|cmd:git status"},
        {"git status -s", "remote_bash|exe|cmd:git status"},
        {"git push", "remote_bash|exe|cmd:git push"},
        {"ls", "remote_bash|exe|cmd:ls"},
        {"", "remote_bash|exe|cmd:"},
    }
    for _, c := range cases {
        args := []byte(fmt.Sprintf(`{"command":%q}`, c.cmd))
        got := makeRuleKey("remote_bash", "exe", args)
        if got != c.want {
            t.Errorf("makeRuleKey(%q) = %q, want %q", c.cmd, got, c.want)
        }
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/ccbroker/tools/ -run "TestGate|TestMakeRuleKey" -v`
Expected: FAIL — `Gate`, `NewGate`, `CheckRequest`, `Event`, `Decision`, `ErrCrossUserDenied`, `ErrPermissionDenied`, `makeRuleKey` not defined.

- [ ] **Step 3: Write Gate implementation**

```go
// internal/ccbroker/tools/permission.go
package tools

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "path/filepath"
    "strings"
    "sync"
    "time"

    "github.com/google/uuid"
)

var (
    ErrCrossUserDenied  = errors.New("cross_user_denied: cross-user executor invocation not allowed")
    ErrPermissionDenied = errors.New("permission_denied")
)

type CheckRequest struct {
    SessionID                 string
    TurnID                    string
    Tool                      string
    ExecutorID                string
    Args                      json.RawMessage
    PermissionMode            string
    SessionCreatorUserID      string
    ExecutorOwnerUserID       string
    ExecutorSharedToWorkspace bool
    Timeout                   time.Duration
}

type Decision struct {
    Verdict string  // "allow" | "deny"
    Scope   string  // "once" | "always"
    By      string
}

type Event struct {
    Type         string          // "permission_request" | "permission_resolved"
    SessionID    string
    TurnID       string
    PermissionID string
    Tool         string
    ExecutorID   string
    Args         json.RawMessage
    Decision     *Decision
    Source       string          // "live" | "sticky"
    EmittedAt    time.Time
}

type Notifier func(sessionID string, evt Event)

type pendingReq struct {
    pid       string
    sessionID string
    turnID    string
    tool      string
    execID    string
    args      json.RawMessage
    deadline  time.Time
    decided   chan Decision
    ruleKey   string
}

type Gate struct {
    notify   Notifier
    mu       sync.Mutex
    pending  map[string]*pendingReq            // pid → req
    sticky   map[string]map[string]Decision    // sessionID → ruleKey → decision
}

func NewGate(notify Notifier) *Gate {
    return &Gate{
        notify:  notify,
        pending: map[string]*pendingReq{},
        sticky:  map[string]map[string]Decision{},
    }
}

func (g *Gate) Check(ctx context.Context, req CheckRequest) error {
    // 1. cross-user
    if req.ExecutorOwnerUserID != "" && req.SessionCreatorUserID != "" &&
        req.ExecutorOwnerUserID != req.SessionCreatorUserID && !req.ExecutorSharedToWorkspace {
        return ErrCrossUserDenied
    }
    // 2. bypass
    if req.PermissionMode == "bypass" {
        return nil
    }
    // 3. sticky
    ruleKey := makeRuleKey(req.Tool, req.ExecutorID, req.Args)
    if d, ok := g.lookupSticky(req.SessionID, ruleKey); ok {
        g.emit(req, Event{Type: "permission_resolved", Decision: &d, Source: "sticky"})
        if d.Verdict == "allow" {
            return nil
        }
        return ErrPermissionDenied
    }
    // 4. ask: emit + block
    pid := "perm_" + uuid.NewString()
    pr := &pendingReq{
        pid: pid, sessionID: req.SessionID, turnID: req.TurnID,
        tool: req.Tool, execID: req.ExecutorID, args: req.Args,
        deadline: time.Now().Add(req.Timeout),
        decided:  make(chan Decision, 1),
        ruleKey:  ruleKey,
    }
    g.mu.Lock()
    g.pending[pid] = pr
    g.mu.Unlock()

    g.emit(req, Event{
        Type: "permission_request", PermissionID: pid,
    })

    var d Decision
    timer := time.NewTimer(time.Until(pr.deadline))
    defer timer.Stop()
    select {
    case d = <-pr.decided:
    case <-timer.C:
        d = Decision{Verdict: "deny", Scope: "timeout"}
    case <-ctx.Done():
        d = Decision{Verdict: "deny", Scope: "cancelled"}
    }

    g.mu.Lock()
    delete(g.pending, pid)
    g.mu.Unlock()

    if d.Verdict == "allow" && d.Scope == "always" {
        g.recordSticky(req.SessionID, ruleKey, d)
    }
    g.emit(req, Event{Type: "permission_resolved", PermissionID: pid, Decision: &d, Source: "live"})

    if d.Verdict == "allow" {
        return nil
    }
    return ErrPermissionDenied
}

func (g *Gate) Resolve(pid string, d Decision) error {
    g.mu.Lock()
    pr, ok := g.pending[pid]
    g.mu.Unlock()
    if !ok {
        return errors.New("already_resolved_or_unknown")
    }
    select {
    case pr.decided <- d:
        return nil
    default:
        return errors.New("already_resolved")
    }
}

func (g *Gate) CancelTurn(turnID string) {
    g.mu.Lock()
    var prs []*pendingReq
    for _, pr := range g.pending {
        if pr.turnID == turnID {
            prs = append(prs, pr)
        }
    }
    g.mu.Unlock()
    for _, pr := range prs {
        select {
        case pr.decided <- Decision{Verdict: "deny", Scope: "cancelled"}:
        default:
        }
    }
}

func (g *Gate) lookupSticky(sessionID, key string) (Decision, bool) {
    g.mu.Lock()
    defer g.mu.Unlock()
    m, ok := g.sticky[sessionID]
    if !ok {
        return Decision{}, false
    }
    d, ok := m[key]
    return d, ok
}

func (g *Gate) recordSticky(sessionID, key string, d Decision) {
    g.mu.Lock()
    defer g.mu.Unlock()
    if g.sticky[sessionID] == nil {
        g.sticky[sessionID] = map[string]Decision{}
    }
    g.sticky[sessionID][key] = d
}

func (g *Gate) emit(req CheckRequest, e Event) {
    e.SessionID = req.SessionID
    e.TurnID = req.TurnID
    if e.Tool == "" {
        e.Tool = req.Tool
    }
    if e.ExecutorID == "" {
        e.ExecutorID = req.ExecutorID
    }
    if e.Args == nil {
        e.Args = req.Args
    }
    if e.EmittedAt.IsZero() {
        e.EmittedAt = time.Now()
    }
    if g.notify != nil {
        g.notify(req.SessionID, e)
    }
}

func makeRuleKey(tool, executorID string, args json.RawMessage) string {
    switch tool {
    case "remote_bash":
        cmd := extractStringField(args, "command")
        toks := strings.Fields(cmd)
        n := len(toks)
        if n > 2 {
            n = 2
        }
        head := strings.Join(toks[:n], " ")
        return fmt.Sprintf("%s|%s|cmd:%s", tool, executorID, head)
    case "remote_read", "remote_write", "remote_edit",
        "remote_glob", "remote_ls", "remote_grep":
        path := extractStringField(args, "file_path")
        if path == "" {
            path = extractStringField(args, "path")
        }
        return fmt.Sprintf("%s|%s|dir:%s", tool, executorID, filepath.Dir(path))
    default:
        return fmt.Sprintf("%s|%s", tool, executorID)
    }
}

func extractStringField(args json.RawMessage, field string) string {
    var m map[string]any
    if err := json.Unmarshal(args, &m); err != nil {
        return ""
    }
    v, _ := m[field].(string)
    return v
}
```

- [ ] **Step 4: Run tests, verify all pass**

Run: `go test ./internal/ccbroker/tools/ -run "TestGate|TestMakeRuleKey" -v -race`
Expected: all PASS, race detector clean.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/tools/permission.go internal/ccbroker/tools/permission_test.go
git commit -m "feat(ccbroker): permission gate with sticky rules and turn cancellation"
```

---

## Task 7: cc-broker — extend ProcessTurnRequest with metadata + thread to tools.Context

**Files:**
- Modify: `internal/ccbroker/handler_turns.go`
- Test: `internal/ccbroker/handler_turns_test.go` (extend)

- [ ] **Step 1: Write failing test for metadata round-trip**

```go
// internal/ccbroker/handler_turns_test.go (append)
func TestProcessTurn_AcceptsMetadata(t *testing.T) {
    // Use the existing handler_turns_test seam: workspaceSetup + runnerRun stubs.
    captured := make(chan *tools.Context, 1)
    origRunner := runnerRun
    runnerRun = func(ctx context.Context, ws *workspace.Workspace, sid, msg string,
        cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
        // The MCP server is built from a tools.Context — we can't reach it directly,
        // so we instead assert on the cfg fields the handler computed.
        ch := make(chan agentsdk.SDKMessage)
        close(ch)
        captured <- &tools.Context{
            ChannelType:         cfg.ChannelType,
            CreatorUserID:       cfg.CreatorUserID,
            PermissionMode:      cfg.PermissionMode,
            PreferredExecutorID: cfg.PreferredExecutorID,
        }
        return ch, nil
    }
    defer func() { runnerRun = origRunner }()

    body := `{
        "session_id":"cse_md","workspace_id":"ws","user_message":"hi",
        "metadata":{"channel_type":"tui","creator_user_id":"u_alice",
                    "permission_mode":"ask","model":"claude-opus-4-7",
                    "preferred_executor_id":"exe_a","turn_kind":"user"}}`
    rr := postJSON(t, server, "/api/turns", body)
    if rr.Code != http.StatusOK {
        t.Fatalf("status %d body=%s", rr.Code, rr.Body)
    }
    select {
    case got := <-captured:
        if got.ChannelType != "tui" || got.CreatorUserID != "u_alice" ||
            got.PermissionMode != "ask" || got.PreferredExecutorID != "exe_a" {
            t.Errorf("metadata not threaded: %+v", got)
        }
    case <-time.After(time.Second):
        t.Fatal("runnerRun never invoked")
    }
}
```

(The existing `handler_turns_test.go` already provides `postJSON` + a `server` fixture; reuse them. If `runner.Config` doesn't yet have these fields, this test will fail to compile — that drives Step 2.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ccbroker/ -run TestProcessTurn_AcceptsMetadata -v`
Expected: FAIL — `runner.Config` lacks the new fields, `ProcessTurnRequest` lacks `Metadata`.

- [ ] **Step 3: Add Metadata struct + extend ProcessTurnRequest**

In `internal/ccbroker/handler_turns.go`:

```go
type TurnMetadata struct {
    ChannelType         string `json:"channel_type,omitempty"`
    CreatorUserID       string `json:"creator_user_id,omitempty"`
    PermissionMode      string `json:"permission_mode,omitempty"`
    Model               string `json:"model,omitempty"`
    PreferredExecutorID string `json:"preferred_executor_id,omitempty"`
    TurnKind            string `json:"turn_kind,omitempty"`  // "user" | "compaction"
}

type ProcessTurnRequest struct {
    SessionID   string       `json:"session_id"`
    WorkspaceID string       `json:"workspace_id"`
    UserMessage string       `json:"user_message"`
    IMChannelID string       `json:"im_channel_id,omitempty"`
    IMUserID    string       `json:"im_user_id,omitempty"`
    Metadata    TurnMetadata `json:"metadata,omitempty"`
}
```

- [ ] **Step 4: Thread metadata into tools.Context build site**

In `handleProcessTurn`, where `tctx := &tools.Context{...}` is built, add:

```go
tctx := &tools.Context{
    SessionID:           req.SessionID,
    WorkspaceID:         req.WorkspaceID,
    IMChannelID:         req.IMChannelID,
    IMUserID:            req.IMUserID,
    // ... existing fields ...

    ChannelType:            defaultStr(req.Metadata.ChannelType, "im"),
    CreatorUserID:          req.Metadata.CreatorUserID,
    PermissionMode:         defaultStr(req.Metadata.PermissionMode, "bypass"),
    PreferredExecutorID:    req.Metadata.PreferredExecutorID,
    Gate:                   s.gate,                       // singleton from server.go (Task 12)
    AgentserverInternalURL: s.config.AgentserverInternalURL,
    CurrentTurnID:          turnID,                       // see Step 5
}
```

`defaultStr` is a small helper:
```go
func defaultStr(v, def string) string {
    if v == "" { return def }
    return v
}
```

- [ ] **Step 5: Generate a turnID per turn and propagate**

Currently the handler doesn't have a turn_id concept. Add one:

```go
turnID := "trn_" + uuid.NewString()
```

near the top of `handleProcessTurn`, after the `req` is decoded. Use it in:
- the user_message event payload (so SSE consumers can correlate)
- `tctx.CurrentTurnID`
- the `runner.Config.TurnID` (Step 6 will add the field)
- the deferred `turn-finished` callback (Task 13)

- [ ] **Step 6: Extend runner.Config**

In `internal/ccbroker/runner/options.go` (next task uses this file too — for now just add the fields):

```go
type Config struct {
    // existing
    MaxTurns int
    // ...

    // new
    SessionID           string
    TurnID              string
    ChannelType         string
    CreatorUserID       string
    PermissionMode      string
    Model               string
    PreferredExecutorID string
    TurnKind            string
    Executors           []ExecutorInfo  // for system prompt; populated by handler from registry list
}

type ExecutorInfo struct {
    ExecutorID  string
    DisplayName string
    Type        string
    Tools       []string
    WorkingDir  string
    Description string
}
```

Pass them through in `handleProcessTurn`:

```go
cfg := runner.Config{
    MaxTurns:            s.config.MaxTurns,
    SessionID:           req.SessionID,
    TurnID:              turnID,
    ChannelType:         tctx.ChannelType,
    CreatorUserID:       tctx.CreatorUserID,
    PermissionMode:      tctx.PermissionMode,
    Model:               req.Metadata.Model,
    PreferredExecutorID: tctx.PreferredExecutorID,
    TurnKind:            req.Metadata.TurnKind,
    Executors:           queryExecutorList(s.config.ExecutorRegistryURL, req.WorkspaceID), // helper
}
```

`queryExecutorList` returns a snapshot of executors for the system prompt; in tests it can be empty. Place it next to `optsFromMetadata` (Task 9).

- [ ] **Step 7: Run test to verify pass**

Run: `go test ./internal/ccbroker/ -run TestProcessTurn_AcceptsMetadata -v`
Expected: PASS.

- [ ] **Step 8: Run full ccbroker package tests for regression**

Run: `go test ./internal/ccbroker/...`
Expected: existing IM regression tests pass (metadata absent = old behavior).

- [ ] **Step 9: Commit**

```bash
git add internal/ccbroker/handler_turns.go internal/ccbroker/runner/options.go internal/ccbroker/handler_turns_test.go
git commit -m "feat(ccbroker): TurnMetadata in ProcessTurnRequest, threaded to tools.Context and runner.Config"
```

---

## Task 8: cc-broker — BuildSystemPrompt with preferred executor + channel hint

**Files:**
- Create: `internal/ccbroker/tools/prompt.go`
- Test: `internal/ccbroker/tools/prompt_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/ccbroker/tools/prompt_test.go
package tools

import (
    "strings"
    "testing"
)

func TestBuildSystemPrompt_PreferredExecutorMarked(t *testing.T) {
    p := BuildSystemPrompt(PromptInput{
        ChannelType:         "tui",
        PreferredExecutorID: "exe_a",
        Executors: []ExecutorInfo{
            {ExecutorID: "exe_a", DisplayName: "Laptop", Type: "local_agent"},
            {ExecutorID: "exe_b", DisplayName: "Sandbox", Type: "sandbox"},
        },
    })
    if !strings.Contains(p, "exe_a") || !strings.Contains(p, "PREFERRED FOR THIS SESSION") {
        t.Errorf("expected preferred marker on exe_a, got:\n%s", p)
    }
    if !strings.Contains(p, "exe_b") {
        t.Errorf("expected non-preferred executor listed, got:\n%s", p)
    }
    if strings.Count(p, "PREFERRED FOR THIS SESSION") != 1 {
        t.Errorf("only one executor should be marked preferred, got %d", strings.Count(p, "PREFERRED FOR THIS SESSION"))
    }
    if !strings.Contains(p, "interactive terminal client") {
        t.Errorf("TUI channel hint missing")
    }
}

func TestBuildSystemPrompt_IMChannelHasIMHint(t *testing.T) {
    p := BuildSystemPrompt(PromptInput{
        ChannelType: "im",
        Executors:   []ExecutorInfo{{ExecutorID: "exe_x", DisplayName: "x", Type: "sandbox"}},
    })
    if !strings.Contains(p, "instant messaging") {
        t.Errorf("IM channel hint missing")
    }
    if strings.Contains(p, "PREFERRED FOR THIS SESSION") {
        t.Errorf("IM channel should not have preferred marker")
    }
}

func TestBuildSystemPrompt_NoExecutors(t *testing.T) {
    p := BuildSystemPrompt(PromptInput{ChannelType: "tui"})
    if !strings.Contains(p, "No execution environments") {
        t.Errorf("expected empty-list note, got:\n%s", p)
    }
}
```

`ExecutorInfo` is the type defined in Task 7 (`runner.ExecutorInfo`). For this package's test, define a local alias OR move `ExecutorInfo` to `internal/ccbroker/tools/`. Move it to `tools` package — it's referenced by both `runner` and `tools`, and `tools` is a leaf in the import graph. Update `runner/options.go` to `import "../tools"` and use `tools.ExecutorInfo`.

- [ ] **Step 2: Run test to verify fail**

Run: `go test ./internal/ccbroker/tools/ -run TestBuildSystemPrompt -v`
Expected: FAIL.

- [ ] **Step 3: Implement prompt.go**

```go
// internal/ccbroker/tools/prompt.go
package tools

import (
    "fmt"
    "strings"
)

type ExecutorInfo struct {
    ExecutorID  string
    DisplayName string
    Type        string
    Tools       []string
    WorkingDir  string
    Description string
}

type PromptInput struct {
    ChannelType         string
    PreferredExecutorID string
    Executors           []ExecutorInfo
}

func BuildSystemPrompt(in PromptInput) string {
    var b strings.Builder
    if len(in.Executors) == 0 {
        b.WriteString("No execution environments are currently registered for this workspace.\n")
    } else {
        b.WriteString("You are operating in a workspace with the following execution environments:\n\n")
        for _, e := range in.Executors {
            marker := ""
            if e.ExecutorID == in.PreferredExecutorID {
                marker = " ★ PREFERRED FOR THIS SESSION"
            }
            fmt.Fprintf(&b, "- %s (id=%s, type=%s)%s\n", e.DisplayName, e.ExecutorID, e.Type, marker)
            if e.Description != "" {
                fmt.Fprintf(&b, "  %s\n", e.Description)
            }
            if e.WorkingDir != "" {
                fmt.Fprintf(&b, "  cwd: %s\n", e.WorkingDir)
            }
        }
    }

    if in.PreferredExecutorID != "" {
        fmt.Fprintf(&b, `
The user is operating from executor %q. Strongly prefer this executor for any
remote_* tool calls unless the task explicitly requires a different environment.
When unsure, ask before routing to a non-preferred executor.
`, in.PreferredExecutorID)
    }

    if in.ChannelType == "tui" {
        b.WriteString(`
The user is interacting through an interactive terminal client. You may use
AskUserQuestion freely for clarifications. The user can see tool calls in real
time and will be prompted to authorize potentially destructive operations.
`)
    } else {
        b.WriteString(`
The user is interacting through an instant messaging channel. Keep responses
concise and avoid asking too many clarifying questions in a row.
`)
    }
    return b.String()
}
```

- [ ] **Step 4: Run test to verify pass**

Run: `go test ./internal/ccbroker/tools/ -run TestBuildSystemPrompt -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/tools/prompt.go internal/ccbroker/tools/prompt_test.go
git commit -m "feat(ccbroker): BuildSystemPrompt with preferred executor and channel hints"
```

---

## Task 9: cc-broker — runner/options.go uses metadata (model, system prompt)

**Files:**
- Modify: `internal/ccbroker/runner/options.go`
- Test: `internal/ccbroker/runner/options_test.go` (extend)

- [ ] **Step 1: Write failing test**

```go
// internal/ccbroker/runner/options_test.go (append)
func TestBuildClientOptions_AppliesMetadata(t *testing.T) {
    cfg := Config{
        SessionID:           "s",
        TurnID:              "t",
        Model:               "claude-opus-4-7",
        PermissionMode:      "ask",
        ChannelType:         "tui",
        PreferredExecutorID: "exe_a",
        Executors: []tools.ExecutorInfo{
            {ExecutorID: "exe_a", DisplayName: "Laptop", Type: "local_agent"},
        },
    }
    opts := buildClientOptions(nil /*ws unused for shape check*/, cfg, nil /*mcp*/)
    // Smoke: should contain a WithModel("claude-opus-4-7") and a WithSystemPrompt(...)
    var hasModel, hasSysPrompt bool
    for _, o := range opts {
        switch o.(type) {
        case withModelOpt:
            hasModel = true
        case withSysPromptOpt:
            hasSysPrompt = true
        }
    }
    if !hasModel { t.Errorf("WithModel not applied") }
    if !hasSysPrompt { t.Errorf("WithSystemPrompt not applied") }
}
```

The actual `agentsdk.QueryOption` types may not be type-assertable; if so, use a different verification: pass a fake `agentsdk.Client` that records `Apply` calls, OR test the helper functions in isolation. **Pick one**: refactor `buildClientOptions` to return a `[]Modifier` where `Modifier interface { Apply(*ClientConfig) }` and assert via a fake. The test above is a sketch; finalize the structure when implementing.

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/ccbroker/runner/ -run TestBuildClientOptions_AppliesMetadata -v`
Expected: FAIL.

- [ ] **Step 3: Implement / extend buildClientOptions**

```go
// internal/ccbroker/runner/options.go (additions)
import (
    agentsdk "github.com/agentserver/claude-agent-sdk-go"
    "github.com/agentserver/agentserver/internal/ccbroker/tools"
)

func buildClientOptions(ws *workspace.Workspace, cfg Config, mcp *agentsdk.McpSdkServer) []agentsdk.QueryOption {
    opts := []agentsdk.QueryOption{
        agentsdk.WithCwd(ws.ProjectDir),
        // ... existing env / resume / mcp / etc ...
        agentsdk.WithPermissionMode(agentsdk.PermissionBypassAll),
        agentsdk.WithAllowDangerouslySkipPermissions(),
        agentsdk.WithMaxTurns(cfg.MaxTurns),
    }
    if cfg.Model != "" {
        opts = append(opts, agentsdk.WithModel(cfg.Model))
    }
    sysPrompt := tools.BuildSystemPrompt(tools.PromptInput{
        ChannelType:         cfg.ChannelType,
        PreferredExecutorID: cfg.PreferredExecutorID,
        Executors:           cfg.Executors,
    })
    opts = append(opts, agentsdk.WithSystemPrompt(sysPrompt))
    return opts
}
```

(Confirm exact `agentsdk` API names against `pkg/agentsdk` — they may be `WithSystemPromptAppend` or similar. If an "append vs replace" choice exists, prefer the variant that **appends to claude code's default**, so the workspace/CLAUDE.md instructions still apply.)

- [ ] **Step 4: Test passes**

Run: `go test ./internal/ccbroker/runner/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/runner/options.go internal/ccbroker/runner/options_test.go
git commit -m "feat(ccbroker): buildClientOptions applies model + system prompt from metadata"
```

---

## Task 10: cc-broker — wrap each remote_* handler with Gate.Check

**Files:**
- Modify: `internal/ccbroker/tools/executor.go`
- Test: `internal/ccbroker/tools/executor_test.go` (extend)

- [ ] **Step 1: Write failing test for gate integration**

```go
// internal/ccbroker/tools/executor_test.go (append)
func TestRemoteBash_BlockedByGateAsk(t *testing.T) {
    notified := make(chan Event, 4)
    g := NewGate(func(_ string, e Event) { notified <- e })
    tctx := &Context{
        SessionID:           "s",
        WorkspaceID:         "ws",
        Gate:                g,
        PermissionMode:      "ask",
        CreatorUserID:       "u",
        CurrentTurnID:       "t1",
        ExecutorRegistryURL: "http://exec-reg-must-not-be-called",
        HTTP:                &http.Client{Timeout: time.Second},
    }
    // executor lookup needs to find owner=u (matching session creator).
    // Stub via test seam (see Step 3 below for the seam definition).
    origLookup := lookupExecutor
    lookupExecutor = func(_ context.Context, _ *Context, _ string) (lookupResult, error) {
        return lookupResult{OwnerUserID: "u", SharedToWorkspace: false}, nil
    }
    defer func() { lookupExecutor = origLookup }()

    tool := byName(executorTools(tctx), "remote_bash")
    done := make(chan *agentsdk.McpToolResult, 1)
    go func() {
        res, _ := tool.Handle(context.Background(), json.RawMessage(
            `{"executor_id":"exe_a","command":"ls"}`))
        done <- res
    }()

    // gate emits permission_request → simulate user clicking deny
    var pid string
    select {
    case ev := <-notified:
        if ev.Type != "permission_request" { t.Fatalf("first event = %s", ev.Type) }
        pid = ev.PermissionID
    case <-time.After(time.Second):
        t.Fatal("no permission_request emitted")
    }
    if err := g.Resolve(pid, Decision{Verdict: "deny", Scope: "once"}); err != nil {
        t.Fatal(err)
    }
    select {
    case res := <-done:
        if res == nil || !res.IsError {
            t.Errorf("expected IsError result, got %+v", res)
        }
        if !strings.Contains(textOf(res), "permission_denied") {
            t.Errorf("error text missing reason code: %s", textOf(res))
        }
    case <-time.After(time.Second):
        t.Fatal("tool didn't return after deny")
    }
}

func TestRemoteBash_BypassDispatchesToRegistry(t *testing.T) {
    var registryHit bool
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == "/api/execute" {
            registryHit = true
            w.Write([]byte(`{"output":"hello","exit_code":0}`))
        }
    }))
    defer srv.Close()

    tctx := &Context{
        SessionID:           "s",
        Gate:                NewGate(func(_ string, _ Event) {}),
        PermissionMode:      "bypass",
        CreatorUserID:       "u",
        ExecutorRegistryURL: srv.URL,
        HTTP:                &http.Client{Timeout: time.Second},
    }
    origLookup := lookupExecutor
    lookupExecutor = func(_ context.Context, _ *Context, _ string) (lookupResult, error) {
        return lookupResult{OwnerUserID: "u"}, nil
    }
    defer func() { lookupExecutor = origLookup }()

    tool := byName(executorTools(tctx), "remote_bash")
    res, err := tool.Handle(context.Background(),
        json.RawMessage(`{"executor_id":"exe_a","command":"ls"}`))
    if err != nil { t.Fatal(err) }
    if res == nil || res.IsError {
        t.Errorf("expected non-error result, got %+v", res)
    }
    if !registryHit {
        t.Errorf("expected dispatch to executor-registry")
    }
}
```

`textOf(res)` and `byName` are existing test helpers — if missing, add small ones in the test file.

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/ccbroker/tools/ -run "TestRemoteBash" -v`
Expected: FAIL — `lookupExecutor` seam undefined; gate not integrated.

- [ ] **Step 3: Add lookup seam + wrap each remote_* handler**

In `internal/ccbroker/tools/executor.go`:

```go
// lookupExecutor fetches owner_user_id + shared_to_workspace from executor-registry.
// Test seam: tests overwrite this var.
type lookupResult struct {
    OwnerUserID       string
    SharedToWorkspace bool
    Online            bool  // for "executor offline" hint; v1 unused, leave false
}

var lookupExecutor = func(ctx context.Context, tctx *Context, executorID string) (lookupResult, error) {
    if tctx.ExecutorRegistryURL == "" {
        return lookupResult{}, fmt.Errorf("executor registry URL not configured")
    }
    req, _ := http.NewRequestWithContext(ctx, "GET",
        tctx.ExecutorRegistryURL+"/api/executors/"+url.PathEscape(executorID), nil)
    resp, err := tctx.HTTP.Do(req)
    if err != nil { return lookupResult{}, err }
    defer resp.Body.Close()
    if resp.StatusCode != 200 {
        return lookupResult{}, fmt.Errorf("executor lookup %d", resp.StatusCode)
    }
    var info struct {
        OwnerUserID       string `json:"owner_user_id"`
        SharedToWorkspace bool   `json:"shared_to_workspace"`
        Status            string `json:"status"`
    }
    json.NewDecoder(resp.Body).Decode(&info)
    return lookupResult{
        OwnerUserID:       info.OwnerUserID,
        SharedToWorkspace: info.SharedToWorkspace,
        Online:            info.Status == "online",
    }, nil
}

// gateCheck wraps each remote_* handler. On deny / timeout / cross-user, returns
// an IsError McpToolResult with reason-code prefix; otherwise returns nil
// signalling the caller may proceed.
func gateCheck(ctx context.Context, tctx *Context, tool, executorID string,
    args json.RawMessage) *agentsdk.McpToolResult {

    info, err := lookupExecutor(ctx, tctx, executorID)
    if err != nil {
        return errResult(fmt.Errorf("executor_unknown: %w", err))
    }
    err = tctx.Gate.Check(ctx, CheckRequest{
        SessionID:                 tctx.SessionID,
        TurnID:                    tctx.CurrentTurnID,
        Tool:                      tool,
        ExecutorID:                executorID,
        Args:                      args,
        PermissionMode:            tctx.PermissionMode,
        SessionCreatorUserID:      tctx.CreatorUserID,
        ExecutorOwnerUserID:       info.OwnerUserID,
        ExecutorSharedToWorkspace: info.SharedToWorkspace,
        Timeout:                   30 * time.Second,
    })
    switch err {
    case nil:
        return nil
    case ErrCrossUserDenied:
        return errResult(fmt.Errorf("cross_user_denied: executor %s belongs to a different user", executorID))
    case ErrPermissionDenied:
        return errResult(fmt.Errorf("permission_denied: user declined %s on %s", tool, executorID))
    default:
        return errResult(fmt.Errorf("permission_error: %w", err))
    }
}
```

Then for each `remote_*` tool definition (e.g., `remote_bash`, `remote_read`, ...), prepend the gate check at handler entry. Concrete change for `remote_bash`:

```go
agentsdk.Tool[bashInput]("remote_bash", "...",
    func(ctx context.Context, in bashInput) (*agentsdk.McpToolResult, error) {
        rawArgs, _ := json.Marshal(in)
        if blocked := gateCheck(ctx, tctx, "remote_bash", in.ExecutorID, rawArgs); blocked != nil {
            return blocked, nil
        }
        // ... existing dispatch to executor-registry /api/execute ...
    })
```

Repeat for `remote_read`, `remote_write`, `remote_edit`, `remote_glob`, `remote_grep`, `remote_ls`. `list_executors` is **not** gated (it's read-only discovery).

- [ ] **Step 4: Verify tests pass**

Run: `go test ./internal/ccbroker/tools/ -run "TestRemoteBash" -v -race`
Expected: both PASS.

- [ ] **Step 5: Run full tools tests**

Run: `go test ./internal/ccbroker/tools/...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ccbroker/tools/executor.go internal/ccbroker/tools/executor_test.go
git commit -m "feat(ccbroker): permission gate wraps every remote_* handler"
```

---

## Task 11: cc-broker — send_* short-circuit on TUI session

**Files:**
- Modify: `internal/ccbroker/tools/im.go`
- Test: `internal/ccbroker/tools/im_test.go` (extend)

- [ ] **Step 1: Write failing test**

```go
// internal/ccbroker/tools/im_test.go (append)
func TestSendMessage_TUIShortCircuit(t *testing.T) {
    var bridgeHit bool
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        bridgeHit = true
        w.WriteHeader(200)
    }))
    defer srv.Close()

    // TUI session: IMChannelID empty, IMBridgeURL set (to confirm we DON'T hit it).
    tctx := &Context{
        IMChannelID: "",
        IMBridgeURL: srv.URL,
        HTTP:        &http.Client{Timeout: time.Second},
    }
    tool := byName(imTools(tctx), "send_message")
    res, err := tool.Handle(context.Background(),
        json.RawMessage(`{"text":"hello tui"}`))
    if err != nil { t.Fatal(err) }
    if res == nil || res.IsError {
        t.Errorf("expected ok result, got %+v", res)
    }
    if bridgeHit {
        t.Errorf("imbridge should NOT be hit when IMChannelID is empty")
    }
}

func TestSendMessage_IMSessionStillCallsBridge(t *testing.T) {
    var bridgeHit bool
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        bridgeHit = true
        w.WriteHeader(200)
    }))
    defer srv.Close()
    tctx := &Context{
        IMChannelID: "ch_1",
        IMUserID:    "u_1",
        IMBridgeURL: srv.URL,
        HTTP:        &http.Client{Timeout: time.Second},
    }
    tool := byName(imTools(tctx), "send_message")
    _, _ = tool.Handle(context.Background(),
        json.RawMessage(`{"text":"hello im"}`))
    if !bridgeHit {
        t.Errorf("imbridge SHOULD be hit for IM session")
    }
}
```

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/ccbroker/tools/ -run TestSendMessage -v`
Expected: existing tests pass; new TUI test fails (currently calls bridge unconditionally).

- [ ] **Step 3: Add short-circuit to each send_* handler**

In `internal/ccbroker/tools/im.go`, at the top of each handler (`send_message`, `send_image`, `send_file`):

```go
agentsdk.Tool[sendMessageInput]("send_message", "...",
    func(ctx context.Context, in sendMessageInput) (*agentsdk.McpToolResult, error) {
        if in.Text == "" {
            return errResult(fmt.Errorf("text is required")), nil
        }
        if tctx.IMChannelID == "" {
            // TUI session — SDK's own tool_use event already conveys text via SSE;
            // imbridge has nothing to deliver to. Ack.
            return &agentsdk.McpToolResult{
                Content: []agentsdk.McpToolContent{{Type: "text", Text: "ok (delivered via session SSE)"}},
            }, nil
        }
        return imbridgePost(ctx, tctx, "/api/internal/imbridge/send", map[string]string{
            "channel_id": tctx.IMChannelID,
            "to_user_id": tctx.IMUserID,
            "text":       in.Text,
        })
    }),
```

Same for `send_image` and `send_file`. (`send_file` currently always errors with "not yet supported" — extend it to ack on TUI sessions too, since the file source travels in args; render handled on TUI side.)

- [ ] **Step 4: Verify tests pass**

Run: `go test ./internal/ccbroker/tools/ -run TestSendMessage -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/tools/im.go internal/ccbroker/tools/im_test.go
git commit -m "feat(ccbroker): send_* short-circuits on TUI sessions (no imbridge call)"
```

---

## Task 12: cc-broker — new HTTP endpoints (cancel, decide, compact, get-active-turn)

**Files:**
- Create: `internal/ccbroker/handler_tui_routes.go`
- Modify: `internal/ccbroker/server.go`
- Test: `internal/ccbroker/handler_tui_routes_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/ccbroker/handler_tui_routes_test.go
package ccbroker

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/agentserver/agentserver/internal/ccbroker/tools"
)

func TestDecidePermission_HappyPath(t *testing.T) {
    srv := newTestServer(t)  // existing helper
    g := srv.gate
    // simulate a pending request
    pid := "perm_xyz"
    g.AddPendingForTest(pid, "s1", "t1")  // helper to be added in tools/permission.go for tests
    body := `{"verdict":"allow","scope":"once"}`
    rr := httptest.NewRecorder()
    req := httptest.NewRequest("POST",
        "/api/sessions/s1/permissions/"+pid+"/decide",
        strings.NewReader(body))
    srv.Routes().ServeHTTP(rr, req)
    if rr.Code != 200 {
        t.Errorf("status %d body=%s", rr.Code, rr.Body)
    }
}

func TestCancelTurn_TerminatesActiveTurn(t *testing.T) {
    srv := newTestServer(t)
    srv.activeTurns.Set("s1", "t1", func() {  /* canceller */ })  // helper
    rr := httptest.NewRecorder()
    req := httptest.NewRequest("POST",
        "/api/sessions/s1/turns/t1/cancel", nil)
    srv.Routes().ServeHTTP(rr, req)
    if rr.Code != http.StatusAccepted {
        t.Errorf("status %d", rr.Code)
    }
}

func TestGetActiveTurn_ReportsCurrent(t *testing.T) {
    srv := newTestServer(t)
    srv.activeTurns.Set("s1", "t99", func() {})
    rr := httptest.NewRecorder()
    req := httptest.NewRequest("GET", "/api/sessions/s1/turns/active", nil)
    srv.Routes().ServeHTTP(rr, req)
    var resp map[string]any
    json.Unmarshal(rr.Body.Bytes(), &resp)
    if resp["turn_id"] != "t99" {
        t.Errorf("turn_id=%v want t99", resp["turn_id"])
    }
}

func TestCompactNow_QueuesForSession(t *testing.T) {
    srv := newTestServer(t)
    rr := httptest.NewRecorder()
    req := httptest.NewRequest("POST", "/api/sessions/s1/compact", nil)
    srv.Routes().ServeHTTP(rr, req)
    if rr.Code != http.StatusAccepted {
        t.Errorf("status %d", rr.Code)
    }
    if !srv.compactQueue.IsSet("s1") {
        t.Errorf("compactQueue should mark s1")
    }
}

// Smoke-test that AddPendingForTest test seam works (compile guard).
func TestGate_AddPendingForTest(t *testing.T) {
    g := tools.NewGate(func(_ string, _ tools.Event) {})
    g.AddPendingForTest("p1", "s", "t")
    // Resolve should now be possible
    if err := g.Resolve("p1", tools.Decision{Verdict: "allow", Scope: "once"}); err != nil {
        t.Errorf("resolve after AddPendingForTest: %v", err)
    }
}
```

This requires:
- `srv.gate`, `srv.activeTurns`, `srv.compactQueue` fields on the cc-broker `Server` struct
- `activeTurns.Set/Get/Cancel`, `compactQueue.Set/IsSet/Take` types
- `tools.Gate.AddPendingForTest(pid, sid, tid)` test seam method

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/ccbroker/ -run "TestDecidePermission|TestCancelTurn|TestGetActiveTurn|TestCompactNow|TestGate_AddPendingForTest" -v`
Expected: all FAIL.

- [ ] **Step 3: Implement activeTurns + compactQueue support types**

In `internal/ccbroker/server.go` (or new `internal/ccbroker/state.go`):

```go
type activeTurnRegistry struct {
    mu  sync.Mutex
    m   map[string]activeTurnEntry  // sessionID → entry
}
type activeTurnEntry struct {
    TurnID  string
    Cancel  func()
}
func (r *activeTurnRegistry) Set(sid, tid string, cancel func()) {
    r.mu.Lock(); defer r.mu.Unlock()
    if r.m == nil { r.m = map[string]activeTurnEntry{} }
    r.m[sid] = activeTurnEntry{TurnID: tid, Cancel: cancel}
}
func (r *activeTurnRegistry) Get(sid string) (string, bool) {
    r.mu.Lock(); defer r.mu.Unlock()
    e, ok := r.m[sid]; return e.TurnID, ok
}
func (r *activeTurnRegistry) Cancel(sid, tid string) bool {
    r.mu.Lock()
    e, ok := r.m[sid]
    r.mu.Unlock()
    if !ok || e.TurnID != tid { return false }
    e.Cancel()
    return true
}
func (r *activeTurnRegistry) Clear(sid, tid string) {
    r.mu.Lock(); defer r.mu.Unlock()
    if e, ok := r.m[sid]; ok && e.TurnID == tid {
        delete(r.m, sid)
    }
}

type compactQueue struct {
    mu  sync.Mutex
    set map[string]struct{}
}
func (c *compactQueue) Set(sid string) {
    c.mu.Lock(); defer c.mu.Unlock()
    if c.set == nil { c.set = map[string]struct{}{} }
    c.set[sid] = struct{}{}
}
func (c *compactQueue) IsSet(sid string) bool {
    c.mu.Lock(); defer c.mu.Unlock()
    _, ok := c.set[sid]; return ok
}
func (c *compactQueue) Take(sid string) bool {  // remove + return whether it was set
    c.mu.Lock(); defer c.mu.Unlock()
    if _, ok := c.set[sid]; ok {
        delete(c.set, sid); return true
    }
    return false
}
```

Add fields to `Server`:
```go
type Server struct {
    // existing
    gate         *tools.Gate
    activeTurns  *activeTurnRegistry
    compactQueue *compactQueue
}
```

Initialize in the constructor:
```go
gate: tools.NewGate(func(sid string, e tools.Event) {
    // emit to cc-broker's SSE consumers (Task 17: agentserver bridges this)
    s.broadcastEvent(sid, e)
}),
activeTurns:  &activeTurnRegistry{},
compactQueue: &compactQueue{},
```

(For now `s.broadcastEvent` writes to whatever SSE channel cc-broker uses; if cc-broker only emits via the SSE response of `/api/turns`, the gate's emit path is the per-turn SSE writer — connect via tctx.EmitSSE callback wired in handler_turns. Concrete wiring is in Task 16.)

- [ ] **Step 4: Add AddPendingForTest seam to Gate**

In `internal/ccbroker/tools/permission.go` (test-only export):

```go
// AddPendingForTest is a test seam — wire up a fake pending request for tests
// that exercise Resolve / CancelTurn without going through Check.
func (g *Gate) AddPendingForTest(pid, sessionID, turnID string) {
    g.mu.Lock()
    defer g.mu.Unlock()
    g.pending[pid] = &pendingReq{
        pid: pid, sessionID: sessionID, turnID: turnID,
        decided: make(chan Decision, 1),
        deadline: time.Now().Add(time.Hour),
    }
}
```

Annotate it as test-only with a comment; if your tooling supports `_test.go`-only exports, move it to `permission_export_test.go`.

- [ ] **Step 5: Implement handler_tui_routes.go**

```go
// internal/ccbroker/handler_tui_routes.go
package ccbroker

import (
    "encoding/json"
    "net/http"

    "github.com/go-chi/chi/v5"

    "github.com/agentserver/agentserver/internal/ccbroker/tools"
)

func (s *Server) handleCancelTurn(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    tid := chi.URLParam(r, "tid")
    s.activeTurns.Cancel(sid, tid)
    s.gate.CancelTurn(tid)
    // emit turn_cancelled (best-effort; the SSE stream may already have closed)
    s.broadcastEvent(sid, tools.Event{
        Type: "turn_cancelled", SessionID: sid, TurnID: tid,
    })
    w.WriteHeader(http.StatusAccepted)
    w.Write([]byte(`{"cancelled":true}`))
}

func (s *Server) handleDecidePermission(w http.ResponseWriter, r *http.Request) {
    pid := chi.URLParam(r, "pid")
    var body struct {
        Verdict string `json:"verdict"`
        Scope   string `json:"scope"`
        By      string `json:"by"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, `{"code":"invalid"}`, http.StatusBadRequest)
        return
    }
    err := s.gate.Resolve(pid, tools.Decision{
        Verdict: body.Verdict, Scope: body.Scope, By: body.By,
    })
    if err != nil {
        http.Error(w, `{"code":"already_resolved"}`, http.StatusConflict)
        return
    }
    w.WriteHeader(http.StatusOK)
    w.Write([]byte(`{"accepted":true}`))
}

func (s *Server) handleCompactNow(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    s.compactQueue.Set(sid)
    w.WriteHeader(http.StatusAccepted)
    w.Write([]byte(`{"queued":true}`))
}

func (s *Server) handleGetActiveTurn(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    if tid, ok := s.activeTurns.Get(sid); ok {
        json.NewEncoder(w).Encode(map[string]string{"turn_id": tid})
        return
    }
    json.NewEncoder(w).Encode(map[string]any{"turn_id": nil})
}
```

- [ ] **Step 6: Register routes in cc-broker server.go**

```go
// in Routes()/Router() of internal/ccbroker/server.go
r.Post("/api/sessions/{sid}/turns/{tid}/cancel", s.handleCancelTurn)
r.Post("/api/sessions/{sid}/permissions/{pid}/decide", s.handleDecidePermission)
r.Post("/api/sessions/{sid}/compact", s.handleCompactNow)
r.Get ("/api/sessions/{sid}/turns/active", s.handleGetActiveTurn)
```

- [ ] **Step 7: Verify tests pass**

Run: `go test ./internal/ccbroker/ -run "TestDecidePermission|TestCancelTurn|TestGetActiveTurn|TestCompactNow|TestGate_AddPendingForTest" -v -race`
Expected: all PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/ccbroker/handler_tui_routes.go internal/ccbroker/server.go internal/ccbroker/tools/permission.go internal/ccbroker/handler_tui_routes_test.go
git commit -m "feat(ccbroker): cancel/decide/compact/get-active-turn HTTP endpoints"
```

---

## Task 13: cc-broker — handler_turns.go threads turnID + defer turn-finished callback

**Files:**
- Modify: `internal/ccbroker/handler_turns.go`
- Test: `internal/ccbroker/handler_turns_test.go` (extend)

- [ ] **Step 1: Write failing test for turn-finished callback**

```go
// internal/ccbroker/handler_turns_test.go (append)
func TestProcessTurn_CallsTurnFinished(t *testing.T) {
    var calledWith map[string]string
    var mu sync.Mutex
    fakeAS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !strings.HasSuffix(r.URL.Path, "/turn-finished") {
            return
        }
        mu.Lock()
        var b map[string]string
        json.NewDecoder(r.Body).Decode(&b)
        calledWith = b
        mu.Unlock()
        w.WriteHeader(200)
    }))
    defer fakeAS.Close()

    server := newTestServer(t)  // newTestServer must inject AgentserverInternalURL=fakeAS.URL
    server.config.AgentserverInternalURL = fakeAS.URL

    body := `{"session_id":"cse_x","workspace_id":"ws","user_message":"hi"}`
    rr := postJSON(t, server, "/api/turns", body)
    if rr.Code != 200 {
        t.Fatalf("status %d", rr.Code)
    }
    deadline := time.Now().Add(2 * time.Second)
    for time.Now().Before(deadline) {
        mu.Lock()
        if calledWith != nil { mu.Unlock(); break }
        mu.Unlock()
        time.Sleep(10 * time.Millisecond)
    }
    mu.Lock()
    defer mu.Unlock()
    if calledWith == nil {
        t.Fatal("turn-finished callback never fired")
    }
    if calledWith["session_id"] != "cse_x" {
        t.Errorf("session_id=%q want cse_x", calledWith["session_id"])
    }
    if calledWith["turn_id"] == "" {
        t.Errorf("turn_id missing")
    }
}
```

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/ccbroker/ -run TestProcessTurn_CallsTurnFinished -v`
Expected: FAIL.

- [ ] **Step 3: Add turnFinishedCallback helper + defer call**

In `internal/ccbroker/handler_turns.go`:

```go
func (s *Server) callTurnFinished(sessionID, turnID string) {
    if s.config.AgentserverInternalURL == "" {
        return
    }
    body, _ := json.Marshal(map[string]string{
        "session_id": sessionID,
        "turn_id":    turnID,
    })
    req, err := http.NewRequest("POST",
        s.config.AgentserverInternalURL+"/internal/sessions/"+sessionID+"/turn-finished",
        bytes.NewReader(body))
    if err != nil { return }
    req.Header.Set("Content-Type", "application/json")
    if s.config.InternalAPISecret != "" {
        req.Header.Set("X-Internal-Secret", s.config.InternalAPISecret)
    }
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    req = req.WithContext(ctx)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        s.logger.Warn("turn-finished callback failed", "session_id", sessionID, "err", err)
        return
    }
    resp.Body.Close()
}
```

In `handleProcessTurn`, after generating `turnID`, register the active turn and add a defer:

```go
turnID := "trn_" + uuid.NewString()

turnCtx, cancelTurn := context.WithCancel(r.Context())
s.activeTurns.Set(req.SessionID, turnID, cancelTurn)
defer func() {
    cancelTurn()
    s.activeTurns.Clear(req.SessionID, turnID)
    s.callTurnFinished(req.SessionID, turnID)
}()
```

Use `turnCtx` (not `r.Context()`) where the SDK runs, so cancel propagates.

- [ ] **Step 4: Add config.AgentserverInternalURL + InternalAPISecret**

In `internal/ccbroker/config.go`:
```go
type Config struct {
    // existing
    AgentserverInternalURL string  `json:"agentserver_internal_url"`  // env: AGENTSERVER_INTERNAL_URL
    InternalAPISecret      string  `json:"internal_api_secret"`       // env: INTERNAL_API_SECRET
}
```
Wire env-var loading in the same place existing config keys are loaded.

- [ ] **Step 5: Verify test passes**

Run: `go test ./internal/ccbroker/ -run TestProcessTurn_CallsTurnFinished -v`
Expected: PASS.

- [ ] **Step 6: Run full ccbroker tests**

Run: `go test ./internal/ccbroker/...`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/ccbroker/handler_turns.go internal/ccbroker/config.go internal/ccbroker/handler_turns_test.go
git commit -m "feat(ccbroker): turnID + activeTurns registration + turn-finished defer callback"
```

---

## Task 14: agentserver — TUI inbound handler with CAS

**Files:**
- Create: `internal/server/handler_tui_inbound.go`
- Create: `internal/server/handler_tui_inbound_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/server/handler_tui_inbound_test.go
package server

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
)

func TestTUIInbound_NewSession_CreatesAndCalls(t *testing.T) {
    s, ccBroker, _ := newTestServerWithFakes(t)  // see helper sketch in Step 3
    defer ccBroker.Close()

    body := `{"executor_id":"exe_a","text":"hello","permission_responder":true,"metadata":{"channel_type":"tui"}}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/workspaces/ws_test/tui/inbound", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusAccepted {
        t.Fatalf("status %d body=%s", rr.Code, rr.Body)
    }
    var resp map[string]any
    json.Unmarshal(rr.Body.Bytes(), &resp)
    if resp["session_id"] == "" || resp["session_id"] == nil {
        t.Errorf("session_id missing in response")
    }
    if resp["turn_id"] == "" || resp["turn_id"] == nil {
        t.Errorf("turn_id missing in response")
    }
    // session in DB
    sid := resp["session_id"].(string)
    sess, _ := s.DB.GetAgentSession(sid)
    if sess == nil { t.Fatal("session not in DB") }
    if sess.ChannelType != "tui" { t.Errorf("channel_type=%q want tui", sess.ChannelType) }
    if sess.PermissionMode != "ask" { t.Errorf("permission_mode=%q want ask", sess.PermissionMode) }
    if sess.PermissionResponder == nil || *sess.PermissionResponder != "exe_a" {
        t.Errorf("responder=%v want exe_a", sess.PermissionResponder)
    }
}

func TestTUIInbound_TurnInProgress_Returns409(t *testing.T) {
    s, ccBroker, _ := newTestServerWithFakes(t)
    defer ccBroker.Close()

    // create a session and pre-claim active turn
    sid := "cse_t"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:exe_a:1",
        CreatorUserID: "u_test", PermissionMode: "ask",
    })
    _, _ = s.DB.ClaimActiveTurn(context.Background(), sid, "trn_existing")

    body := `{"session_id":"` + sid + `","executor_id":"exe_a","text":"hi"}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/workspaces/ws_test/tui/inbound", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusConflict {
        t.Errorf("status %d want 409", rr.Code)
    }
    if !strings.Contains(rr.Body.String(), "turn_in_progress") {
        t.Errorf("body missing code: %s", rr.Body)
    }
}
```

`newTestServerWithFakes` returns `(*Server, *httptest.Server /*cc-broker*/, *httptest.Server /*imbridge*/)`. Sketch in Step 3. `mustAuthRequest` adds an OAuth bearer header that the server's auth middleware accepts in tests (use existing test setup pattern from other server tests).

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/server/ -run TestTUIInbound -v`
Expected: FAIL.

- [ ] **Step 3: Implement handler**

```go
// internal/server/handler_tui_inbound.go
package server

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"

    "github.com/agentserver/agentserver/internal/db"
)

const tuiAttachmentMaxBytes = 8 << 20

type tuiInboundReq struct {
    SessionID           string                 `json:"session_id"`
    ExecutorID          string                 `json:"executor_id"`
    Text                string                 `json:"text"`
    Attachments         []tuiInboundAttachment `json:"attachments"`
    Metadata            map[string]any         `json:"metadata"`
    PermissionResponder bool                   `json:"permission_responder"`
}

type tuiInboundAttachment struct {
    Kind        string `json:"kind"`
    Filename    string `json:"filename"`
    Size        int    `json:"size"`
    ContentB64  string `json:"content_b64"`
}

func (s *Server) handleTUIInbound(w http.ResponseWriter, r *http.Request) {
    workspaceID := chi.URLParam(r, "wid")
    userID := authUserID(r)  // from auth middleware

    var req tuiInboundReq
    if err := json.NewDecoder(io.LimitReader(r.Body, tuiAttachmentMaxBytes+1<<10)).Decode(&req); err != nil {
        writeAPIErr(w, http.StatusBadRequest, "invalid", "invalid body")
        return
    }
    if req.ExecutorID == "" || req.Text == "" {
        writeAPIErr(w, http.StatusBadRequest, "invalid", "executor_id and text required")
        return
    }
    var attachBytes int
    for _, a := range req.Attachments {
        attachBytes += len(a.ContentB64)
    }
    if attachBytes > tuiAttachmentMaxBytes {
        writeAPIErr(w, http.StatusRequestEntityTooLarge, "attachment_too_large", "attachments exceed 8 MiB")
        return
    }

    // Resolve / create session
    sid := req.SessionID
    if sid == "" {
        sid = "cse_" + uuid.NewString()
        if err := s.DB.CreateAgentSessionTUI(r.Context(), db.CreateTUISessionParams{
            ID:                  sid,
            WorkspaceID:         workspaceID,
            ExternalID:          fmt.Sprintf("tui:%s:%d", req.ExecutorID, nowUnix()),
            Title:               "TUI session",
            CreatorUserID:       userID,
            PermissionMode:      "ask",
            PreferredExecutorID: req.ExecutorID,
        }); err != nil {
            writeAPIErr(w, 500, "internal", "create session failed")
            return
        }
        if req.PermissionResponder {
            if _, err := s.DB.AttachResponder(r.Context(), sid, req.ExecutorID, true); err != nil {
                log.Printf("tui_inbound: attach responder: %v", err)
            }
        }
    } else {
        sess, err := s.DB.GetAgentSession(sid)
        if err != nil || sess == nil || sess.WorkspaceID != workspaceID {
            writeAPIErr(w, http.StatusNotFound, "not_found", "session not found")
            return
        }
        // observer guard
        if sess.PreferredExecutorID != nil && *sess.PreferredExecutorID != req.ExecutorID {
            writeAPIErr(w, http.StatusForbidden, "not_operator", "this executor is not the operator")
            return
        }
    }

    // CAS active_turn_id
    turnID := "trn_" + uuid.NewString()
    ok, err := s.DB.ClaimActiveTurn(r.Context(), sid, turnID)
    if err != nil {
        writeAPIErr(w, 500, "internal", "claim turn failed")
        return
    }
    if !ok {
        cur, _ := s.DB.GetActiveTurn(r.Context(), sid)
        writeAPIErr(w, http.StatusConflict, "turn_in_progress", fmt.Sprintf("active turn %s", cur))
        return
    }

    // Asynchronously call cc-broker; events flow back via SSE bridging (Task 17).
    go s.callCCBrokerForTUI(context.Background(), sid, turnID, workspaceID, userID, req)

    w.WriteHeader(http.StatusAccepted)
    json.NewEncoder(w).Encode(map[string]any{
        "session_id": sid,
        "turn_id":    turnID,
    })
}

// callCCBrokerForTUI POSTs cc-broker /api/turns and bridges its SSE stream into
// agentserver's per-session SSEBroker. Implementation completed in Task 17;
// stub here just so this handler links.
func (s *Server) callCCBrokerForTUI(ctx context.Context, sid, turnID, wid, userID string, req tuiInboundReq) {
    // Read sticky fields off the (now-saved) session.
    sess, _ := s.DB.GetAgentSession(sid)
    metaModel, _ := req.Metadata["model"].(string)
    if metaModel == "" && sess != nil && sess.PreferredModel != nil {
        metaModel = *sess.PreferredModel
    }
    turnKind, _ := req.Metadata["turn_kind"].(string)
    if turnKind == "" {
        turnKind = "user"
    }
    body, _ := json.Marshal(map[string]any{
        "session_id":   sid,
        "workspace_id": wid,
        "user_message": req.Text,
        "metadata": map[string]any{
            "channel_type":          "tui",
            "creator_user_id":       userID,
            "permission_mode":       sess.PermissionMode,
            "model":                 metaModel,
            "preferred_executor_id": sess.PreferredExecutorID,
            "turn_kind":             turnKind,
        },
    })
    httpReq, _ := http.NewRequestWithContext(ctx, "POST",
        s.CCBrokerURL+"/api/turns", bytes.NewReader(body))
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("Accept", "text/event-stream")
    resp, err := http.DefaultClient.Do(httpReq)
    if err != nil {
        log.Printf("tui_inbound: cc-broker call failed: %v", err)
        _ = s.DB.ClearActiveTurn(ctx, sid, turnID)
        return
    }
    defer resp.Body.Close()
    // The actual SSE pump lives in handler_tui_events.go (Task 17). For now,
    // drain the body so cc-broker can flush + complete the turn.
    _, _ = io.Copy(io.Discard, resp.Body)
}

func writeAPIErr(w http.ResponseWriter, status int, code, msg string) {
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(map[string]any{
        "error": map[string]string{"code": code, "message": msg},
    })
}

func nowUnix() int64 { return time.Now().Unix() }

// authUserID extracts the authenticated user_id from request context.
// Wired by the existing s.Auth.Middleware. If your codebase exposes it as
// `r.Context().Value(userIDKey)`, use that; pseudocode here.
func authUserID(r *http.Request) string {
    // adapt to existing auth context shape
    return "" // replaced in real implementation
}
```

(`authUserID` and `mustAuthRequest` test helper must align with the existing auth middleware. Check `internal/auth` for the actual context key. If unsure, add a `// TODO: wire to real auth context` comment and stub returning a fixed test value behind an env var.)

- [ ] **Step 4: Wire route in server.go**

```go
// internal/server/server.go — inside the authenticated user routes group
r.Post("/api/workspaces/{wid}/tui/inbound", s.handleTUIInbound)
```

- [ ] **Step 5: Verify tests pass**

Run: `go test ./internal/server/ -run TestTUIInbound -v`
Expected: both PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/handler_tui_inbound.go internal/server/handler_tui_inbound_test.go internal/server/server.go
git commit -m "feat(server): /api/workspaces/{wid}/tui/inbound with CAS active_turn_id"
```

---

## Task 15: agentserver — agent-sessions create / attach / list endpoints

**Files:**
- Create: `internal/server/handler_tui_session.go`
- Create: `internal/server/handler_tui_session_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/server/handler_tui_session_test.go
func TestCreateAgentSession_TUI(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    body := `{"workspace_id":"ws_test","executor_id":"exe_a","permission_mode":"ask","preferred_executor_id":"exe_a"}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusCreated {
        t.Fatalf("status %d body=%s", rr.Code, rr.Body)
    }
    var resp map[string]any
    json.Unmarshal(rr.Body.Bytes(), &resp)
    sid := resp["session_id"].(string)
    sess, _ := s.DB.GetAgentSession(sid)
    if sess.ChannelType != "tui" || sess.PermissionMode != "ask" {
        t.Errorf("session: %+v", sess)
    }
}

func TestAttachAgentSession_OperatorMode(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_a"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1",
        CreatorUserID: "u_test", PermissionMode: "ask", PreferredExecutorID: "exe_old",
    })
    _, _ = s.DB.AttachResponder(context.Background(), sid, "exe_old", true)

    body := `{"executor_id":"exe_new","mode":"operator","as_permission_responder":true,"also_become_preferred":true}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/attach", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusOK {
        t.Fatalf("status %d", rr.Code)
    }
    var resp map[string]any
    json.Unmarshal(rr.Body.Bytes(), &resp)
    if resp["previous_responder"] != "exe_old" {
        t.Errorf("previous_responder=%v want exe_old", resp["previous_responder"])
    }
    sess, _ := s.DB.GetAgentSession(sid)
    if *sess.PermissionResponder != "exe_new" {
        t.Errorf("responder=%v want exe_new", sess.PermissionResponder)
    }
    if *sess.PreferredExecutorID != "exe_new" {
        t.Errorf("preferred=%v want exe_new", sess.PreferredExecutorID)
    }
}

func TestAttachAgentSession_ObserverMode_LeavesResponder(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_obs"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1",
        CreatorUserID: "u_test", PermissionMode: "ask", PreferredExecutorID: "exe_op",
    })
    _, _ = s.DB.AttachResponder(context.Background(), sid, "exe_op", true)

    body := `{"executor_id":"exe_obs","mode":"observer"}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/attach", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusOK {
        t.Fatalf("status %d", rr.Code)
    }
    sess, _ := s.DB.GetAgentSession(sid)
    if *sess.PermissionResponder != "exe_op" {
        t.Errorf("observer attach should not change responder, got %v", sess.PermissionResponder)
    }
}

func TestListAgentSessions_FiltersByExecutor(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: "cse_a", WorkspaceID: "ws_test", ExternalID: "tui:exe_x:1",
        CreatorUserID: "u_test", PermissionMode: "ask",
    })
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: "cse_b", WorkspaceID: "ws_test", ExternalID: "tui:exe_y:1",
        CreatorUserID: "u_test", PermissionMode: "ask",
    })
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "GET",
        "/api/agent-sessions?workspace_id=ws_test&channel_type=tui&executor_id=exe_x&latest=1", "")
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusOK {
        t.Fatalf("status %d", rr.Code)
    }
    var resp struct {
        Sessions []map[string]any `json:"sessions"`
    }
    json.Unmarshal(rr.Body.Bytes(), &resp)
    if len(resp.Sessions) != 1 || resp.Sessions[0]["session_id"] != "cse_a" {
        t.Errorf("expected exactly cse_a, got %+v", resp.Sessions)
    }
}
```

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/server/ -run "TestCreateAgentSession|TestAttachAgentSession|TestListAgentSessions" -v`
Expected: FAIL.

- [ ] **Step 3: Implement handlers**

```go
// internal/server/handler_tui_session.go
package server

import (
    "encoding/json"
    "fmt"
    "net/http"
    "strconv"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"

    "github.com/agentserver/agentserver/internal/db"
)

type createSessionReq struct {
    WorkspaceID         string `json:"workspace_id"`
    ExecutorID          string `json:"executor_id"`
    Title               string `json:"title"`
    PermissionMode      string `json:"permission_mode"`
    PreferredExecutorID string `json:"preferred_executor_id"`
}

func (s *Server) handleCreateAgentSession(w http.ResponseWriter, r *http.Request) {
    var req createSessionReq
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeAPIErr(w, 400, "invalid", "invalid body")
        return
    }
    if req.WorkspaceID == "" || req.ExecutorID == "" {
        writeAPIErr(w, 400, "invalid", "workspace_id and executor_id required")
        return
    }
    sid := "cse_" + uuid.NewString()
    extID := fmt.Sprintf("tui:%s:%d", req.ExecutorID, nowUnix())
    title := req.Title
    if title == "" { title = "TUI session" }
    err := s.DB.CreateAgentSessionTUI(r.Context(), db.CreateTUISessionParams{
        ID:                  sid,
        WorkspaceID:         req.WorkspaceID,
        ExternalID:          extID,
        Title:               title,
        CreatorUserID:       authUserID(r),
        PermissionMode:      req.PermissionMode,
        PreferredExecutorID: req.PreferredExecutorID,
    })
    if err != nil {
        writeAPIErr(w, 500, "internal", "create failed")
        return
    }
    w.WriteHeader(http.StatusCreated)
    json.NewEncoder(w).Encode(map[string]any{
        "session_id":   sid,
        "external_id":  extID,
        "channel_type": "tui",
    })
}

type attachSessionReq struct {
    ExecutorID            string `json:"executor_id"`
    Mode                  string `json:"mode"`  // "operator" | "observer"
    AsPermissionResponder bool   `json:"as_permission_responder"`
    AlsoBecomePreferred   bool   `json:"also_become_preferred"`
}

func (s *Server) handleAttachAgentSession(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    var req attachSessionReq
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeAPIErr(w, 400, "invalid", "invalid body")
        return
    }
    if req.ExecutorID == "" {
        writeAPIErr(w, 400, "invalid", "executor_id required")
        return
    }
    sess, err := s.DB.GetAgentSession(sid)
    if err != nil || sess == nil {
        writeAPIErr(w, 404, "not_found", "session not found")
        return
    }
    // Observer = read-only attach: no DB writes; just confirm it exists.
    if req.Mode == "observer" {
        json.NewEncoder(w).Encode(map[string]any{
            "session_id":           sid,
            "permission_responder": sess.PermissionResponder,
            "previous_responder":   "",
            "previous_preferred":   "",
        })
        return
    }
    // Operator (default) — atomic responder + preferred swap
    prev, err := s.DB.AttachResponder(r.Context(), sid, req.ExecutorID, req.AlsoBecomePreferred)
    if err != nil {
        writeAPIErr(w, 500, "internal", "attach failed")
        return
    }
    json.NewEncoder(w).Encode(map[string]any{
        "session_id":           sid,
        "permission_responder": req.ExecutorID,
        "previous_responder":   prev.PreviousResponder,
        "previous_preferred":   prev.PreviousPreferred,
    })
}

func (s *Server) handleListAgentSessions(w http.ResponseWriter, r *http.Request) {
    q := r.URL.Query()
    wid := q.Get("workspace_id")
    chType := q.Get("channel_type")
    execID := q.Get("executor_id")
    if wid == "" || chType == "" {
        writeAPIErr(w, 400, "invalid", "workspace_id and channel_type required")
        return
    }
    limit, _ := strconv.Atoi(q.Get("latest"))
    if limit <= 0 { limit = 20 }
    list, err := s.DB.ListSessionsByChannel(r.Context(), wid, chType, execID, limit)
    if err != nil {
        writeAPIErr(w, 500, "internal", "list failed")
        return
    }
    out := make([]map[string]any, 0, len(list))
    for _, it := range list {
        out = append(out, map[string]any{
            "session_id":           it.ID,
            "external_id":          it.ExternalID,
            "title":                it.Title,
            "last_activity_at":     it.LastActivityAt,
            "permission_responder": it.PermissionResponder,
        })
    }
    json.NewEncoder(w).Encode(map[string]any{"sessions": out})
}
```

- [ ] **Step 4: Wire routes in server.go**

```go
// inside authenticated routes group
r.Post("/api/agent-sessions",                     s.handleCreateAgentSession)
r.Post("/api/agent-sessions/{sid}/attach",        s.handleAttachAgentSession)
r.Get ("/api/agent-sessions",                     s.handleListAgentSessions)
```

- [ ] **Step 5: Verify tests pass**

Run: `go test ./internal/server/ -run "TestCreateAgentSession|TestAttachAgentSession|TestListAgentSessions" -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/handler_tui_session.go internal/server/handler_tui_session_test.go internal/server/server.go
git commit -m "feat(server): /api/agent-sessions create/attach/list endpoints"
```

---

## Task 16: agentserver — SSE events endpoint + bridging from cc-broker turn stream

**Files:**
- Create: `internal/server/handler_tui_events.go`
- Modify: `internal/server/handler_tui_inbound.go` (replace io.Discard with bridge pump)
- Create: `internal/server/handler_tui_events_test.go`

This is the trickiest piece — re-uses `internal/bridge/SSEBroker` to fan-out events from the cc-broker `/api/turns` SSE response to all TUI clients subscribed to the session.

- [ ] **Step 1: Write failing test (end-to-end SSE round-trip)**

```go
// internal/server/handler_tui_events_test.go
func TestTUIEvents_BridgesCCBrokerSSE(t *testing.T) {
    // Fake cc-broker that responds to /api/turns with three SSE events.
    ccBroker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !strings.HasSuffix(r.URL.Path, "/api/turns") {
            return
        }
        w.Header().Set("Content-Type", "text/event-stream")
        f := w.(http.Flusher)
        for i, ev := range []string{"tool_use", "tool_result", "turn_done"} {
            fmt.Fprintf(w, "event: %s\ndata: {\"seq\":%d}\n\n", ev, i+1)
            f.Flush()
        }
    }))
    defer ccBroker.Close()

    s, _, _ := newTestServerWithFakes(t)
    s.CCBrokerURL = ccBroker.URL

    // Pre-create a session
    sid := "cse_evt"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1",
        CreatorUserID: "u_test", PermissionMode: "ask",
    })

    // Subscribe SSE in a goroutine
    received := make(chan string, 8)
    go func() {
        rr := httptest.NewRecorder()
        req := mustAuthRequest(t, "GET", "/api/agent-sessions/"+sid+"/events", "")
        // Use a context with cancel so we can stop the SSE consumer
        ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
        req = req.WithContext(ctx)
        defer cancel()
        s.Router().ServeHTTP(rr, req)
        for _, line := range strings.Split(rr.Body.String(), "\n") {
            if strings.HasPrefix(line, "event: ") {
                received <- strings.TrimPrefix(line, "event: ")
            }
        }
        close(received)
    }()
    time.Sleep(50 * time.Millisecond) // let the subscriber connect

    // Trigger inbound which calls cc-broker → bridges events
    body := `{"executor_id":"exe_a","text":"hi","permission_responder":true}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/workspaces/ws_test/tui/inbound", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusAccepted {
        t.Fatalf("inbound %d", rr.Code)
    }
    // Expect to see tool_use, tool_result, turn_done in the SSE stream
    seen := map[string]bool{}
    deadline := time.After(2 * time.Second)
    for len(seen) < 3 {
        select {
        case ev, ok := <-received:
            if !ok { break }
            seen[ev] = true
        case <-deadline:
            t.Fatalf("timed out, seen=%v", seen)
        }
    }
    for _, want := range []string{"tool_use", "tool_result", "turn_done"} {
        if !seen[want] { t.Errorf("missing event %q", want) }
    }
}
```

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/server/ -run TestTUIEvents_BridgesCCBrokerSSE -v -timeout 10s`
Expected: FAIL — SSE handler doesn't exist; inbound's cc-broker reader discards events.

- [ ] **Step 3: Implement SSE handler**

```go
// internal/server/handler_tui_events.go
package server

import (
    "encoding/json"
    "fmt"
    "net/http"
    "strconv"
    "time"

    "github.com/go-chi/chi/v5"

    "github.com/agentserver/agentserver/internal/bridge"
)

func (s *Server) handleTUIEventStream(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    sess, err := s.DB.GetAgentSession(sid)
    if err != nil || sess == nil {
        writeAPIErr(w, 404, "not_found", "session not found")
        return
    }
    // SSE setup
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming unsupported", http.StatusInternalServerError)
        return
    }
    // Replay backlog
    sinceSeq, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
    if hdr := r.Header.Get("Last-Event-ID"); hdr != "" {
        if v, err := strconv.ParseInt(hdr, 10, 64); err == nil { sinceSeq = v }
    }
    if sinceSeq > 0 {
        events, _ := s.DB.GetAgentSessionEventsSince(sid, sinceSeq, 500)
        for _, ev := range events {
            fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n",
                ev.SequenceNum, ev.EventType, ev.Payload)
        }
        flusher.Flush()
    } else if tail, _ := strconv.Atoi(r.URL.Query().Get("tail")); tail > 0 {
        events, _ := s.DB.GetAgentSessionEventsTail(sid, tail)  // add this query (small wrapper)
        for _, ev := range events {
            fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n",
                ev.SequenceNum, ev.EventType, ev.Payload)
        }
        flusher.Flush()
    }

    // Subscribe to live broker
    sub := s.BridgeHandler.SSE.Subscribe(sid)
    defer s.BridgeHandler.SSE.Unsubscribe(sid, sub)

    keepalive := time.NewTicker(15 * time.Second)
    defer keepalive.Stop()
    ttlRefresh := time.NewTicker(30 * time.Second)
    defer ttlRefresh.Stop()

    for {
        select {
        case <-r.Context().Done():
            return
        case ev := <-sub.Ch:
            if ev == nil { return }
            // event payload aligns with bridge.StreamClientEvent: id + type + data
            fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n",
                ev.ID, ev.Type, ev.Data)
            flusher.Flush()
        case <-keepalive.C:
            if _, err := w.Write([]byte(": keepalive\n\n")); err != nil { return }
            flusher.Flush()
        case <-ttlRefresh.C:
            // touch responder_attached_at for the ghost-cleanup worker
            // (only if this connection is the responder)
            sess2, err := s.DB.GetAgentSession(sid)
            if err == nil && sess2 != nil && sess2.PermissionResponder != nil {
                _, _ = s.DB.AttachResponder(r.Context(), sid, *sess2.PermissionResponder, false)
            }
        }
    }
}
```

Add the small extension `GetAgentSessionEventsTail` to `internal/db/agent_sessions.go`:
```go
func (db *DB) GetAgentSessionEventsTail(sessionID string, n int) ([]AgentSessionEvent, error) {
    rows, err := db.Query(`
        SELECT id, session_id, event_id, event_type, source, epoch, payload, created_at
          FROM agent_session_events
         WHERE session_id = $1
         ORDER BY id DESC LIMIT $2`, sessionID, n)
    /* scan into reverse-ordered slice, then reverse */
}
```

- [ ] **Step 4: Replace io.Discard in handler_tui_inbound.go with SSE pump**

In `internal/server/handler_tui_inbound.go`, rewrite `callCCBrokerForTUI` body to parse SSE frames from cc-broker and forward each to `s.BridgeHandler.SSE.Publish(sid, &bridge.StreamClientEvent{...})`. Persist each event to `agent_session_events` so the SSE replay path (Step 3) sees it.

```go
// (replace the existing io.Copy(io.Discard, ...) block)
sc := bufio.NewScanner(resp.Body)
sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
var (
    eventType string
    dataBuf   bytes.Buffer
    idStr     string
)
flushEvent := func() {
    if eventType == "" && dataBuf.Len() == 0 { return }
    payload := dataBuf.Bytes()
    // persist
    seqRows, _ := s.DB.InsertAgentSessionEvents(sid, []db.AgentSessionEvent{
        {EventID: uuid.NewString(), EventType: eventType, Source: "ccbroker", Payload: append([]byte(nil), payload...)},
    })
    var seq int64
    if len(seqRows) > 0 { seq = seqRows[0].Event.SequenceNum }
    // broadcast
    s.BridgeHandler.SSE.Publish(sid, &bridge.StreamClientEvent{
        ID: seq, Type: eventType, Data: payload,
    })
    eventType = ""
    dataBuf.Reset()
    idStr = ""
}
for sc.Scan() {
    line := sc.Text()
    switch {
    case line == "":
        flushEvent()
    case strings.HasPrefix(line, "event: "):
        eventType = strings.TrimPrefix(line, "event: ")
    case strings.HasPrefix(line, "data: "):
        if dataBuf.Len() > 0 { dataBuf.WriteByte('\n') }
        dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
    case strings.HasPrefix(line, "id: "):
        idStr = strings.TrimPrefix(line, "id: ")
        _ = idStr  // currently unused; cc-broker generates IDs we'll override on persist
    }
}
flushEvent()
_ = s.DB.ClearActiveTurn(ctx, sid, turnID)  // safety net if cc-broker turn-finished missed
```

(`InsertAgentSessionEvents` already exists per `internal/db/agent_sessions.go:121`. `bridge.StreamClientEvent` exists per `internal/bridge/sse.go:8`.)

- [ ] **Step 5: Wire SSE route**

```go
// inside authenticated routes group
r.Get("/api/agent-sessions/{sid}/events", s.handleTUIEventStream)
```

- [ ] **Step 6: Run test to verify pass**

Run: `go test ./internal/server/ -run TestTUIEvents_BridgesCCBrokerSSE -v -timeout 10s`
Expected: PASS.

- [ ] **Step 7: Run full server tests**

Run: `go test ./internal/server/...`
Expected: all PASS (including existing IM regressions).

- [ ] **Step 8: Commit**

```bash
git add internal/server/handler_tui_events.go internal/server/handler_tui_inbound.go internal/server/handler_tui_events_test.go internal/server/server.go internal/db/agent_sessions.go
git commit -m "feat(server): TUI session SSE endpoint, bridges cc-broker turn stream into per-session broker"
```

---

## Task 17: agentserver — /control endpoint with command dispatch

**Files:**
- Create: `internal/server/handler_tui_control.go`
- Create: `internal/server/handler_tui_control_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/server/handler_tui_control_test.go
func TestControl_ModelWritesPreference(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_m"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1",
        CreatorUserID: "u_test", PermissionMode: "ask",
    })
    body := `{"command":"model","args":{"model":"claude-haiku-4-5"}}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/control", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != 200 { t.Fatalf("status %d body=%s", rr.Code, rr.Body) }
    sess, _ := s.DB.GetAgentSession(sid)
    if sess.PreferredModel == nil || *sess.PreferredModel != "claude-haiku-4-5" {
        t.Errorf("preferred_model=%v want claude-haiku-4-5", sess.PreferredModel)
    }
}

func TestControl_PermissionWritesMode(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_p"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1",
        CreatorUserID: "u_test", PermissionMode: "ask",
    })
    body := `{"command":"permission","args":{"mode":"bypass"}}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/control", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != 200 { t.Fatalf("status %d", rr.Code) }
    sess, _ := s.DB.GetAgentSession(sid)
    if sess.PermissionMode != "bypass" {
        t.Errorf("permission_mode=%q want bypass", sess.PermissionMode)
    }
}

func TestControl_CompactProxiesToCCBroker(t *testing.T) {
    var hit bool
    cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if strings.HasSuffix(r.URL.Path, "/compact") { hit = true }
        w.WriteHeader(http.StatusAccepted)
    }))
    defer cc.Close()
    s, _, _ := newTestServerWithFakes(t)
    s.CCBrokerURL = cc.URL
    sid := "cse_c"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1",
        CreatorUserID: "u_test", PermissionMode: "ask",
    })
    body := `{"command":"compact"}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/control", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != 200 { t.Errorf("status %d", rr.Code) }
    if !hit { t.Errorf("cc-broker compact should have been called") }
}

func TestControl_UnknownCommand(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_u"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1",
        CreatorUserID: "u_test", PermissionMode: "ask",
    })
    body := `{"command":"nonsense"}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/control", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusBadRequest {
        t.Errorf("status %d want 400", rr.Code)
    }
}
```

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/server/ -run TestControl -v`
Expected: FAIL.

- [ ] **Step 3: Implement handler**

```go
// internal/server/handler_tui_control.go
package server

import (
    "bytes"
    "context"
    "encoding/json"
    "io"
    "net/http"
    "time"

    "github.com/go-chi/chi/v5"
)

type controlReq struct {
    Command string         `json:"command"`
    Args    map[string]any `json:"args"`
}

func (s *Server) handleAgentSessionControl(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    sess, err := s.DB.GetAgentSession(sid)
    if err != nil || sess == nil {
        writeAPIErr(w, 404, "not_found", "session not found")
        return
    }
    var req controlReq
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeAPIErr(w, 400, "invalid", "invalid body")
        return
    }
    switch req.Command {
    case "model":
        m, _ := req.Args["model"].(string)
        if m == "" {
            writeAPIErr(w, 400, "invalid", "args.model required")
            return
        }
        if _, err := s.DB.Exec(`UPDATE agent_sessions SET preferred_model=$1, updated_at=NOW() WHERE id=$2`, m, sid); err != nil {
            writeAPIErr(w, 500, "internal", "write failed")
            return
        }
        json.NewEncoder(w).Encode(map[string]any{"applied": true, "model": m})
    case "permission":
        m, _ := req.Args["mode"].(string)
        if m != "ask" && m != "bypass" {
            writeAPIErr(w, 400, "invalid", "args.mode must be ask or bypass")
            return
        }
        if _, err := s.DB.Exec(`UPDATE agent_sessions SET permission_mode=$1, updated_at=NOW() WHERE id=$2`, m, sid); err != nil {
            writeAPIErr(w, 500, "internal", "write failed")
            return
        }
        json.NewEncoder(w).Encode(map[string]any{"applied": true, "mode": m})
    case "compact":
        ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
        defer cancel()
        url := s.CCBrokerURL + "/api/sessions/" + sid + "/compact"
        rq, _ := http.NewRequestWithContext(ctx, "POST", url, nil)
        resp, err := http.DefaultClient.Do(rq)
        if err != nil {
            writeAPIErr(w, 502, "upstream", "cc-broker unreachable")
            return
        }
        resp.Body.Close()
        json.NewEncoder(w).Encode(map[string]any{"queued": true})
    case "cost":
        // Sum token usage from agent_session_events. v1 simple: scan latest 1000 events
        // and sum any payload.usage.{input_tokens,output_tokens}. Real cost calc is
        // model-dependent; v1 returns raw token counts and lets the TUI compute.
        events, _ := s.DB.GetAgentSessionEventsSince(sid, 0, 1000)
        var inTok, outTok int64
        for _, ev := range events {
            var p struct{ Usage struct{ InputTokens, OutputTokens int64 } }
            _ = json.Unmarshal(ev.Payload, &p)
            inTok += p.Usage.InputTokens
            outTok += p.Usage.OutputTokens
        }
        json.NewEncoder(w).Encode(map[string]any{
            "input_tokens": inTok, "output_tokens": outTok,
        })
    case "agents":
        // Proxy to executor-registry.
        url := s.ExecutorRegistryURL + "/api/executors?workspace_id=" + sess.WorkspaceID
        rq, _ := http.NewRequestWithContext(r.Context(), "GET", url, nil)
        resp, err := http.DefaultClient.Do(rq)
        if err != nil {
            writeAPIErr(w, 502, "upstream", "executor-registry unreachable")
            return
        }
        defer resp.Body.Close()
        body, _ := io.ReadAll(resp.Body)
        w.Header().Set("Content-Type", "application/json")
        w.Write(body)
    default:
        writeAPIErr(w, 400, "unknown_command", "unknown control command: "+req.Command)
    }
    _ = bytes.MinRead  // keep imports happy if unused
}
```

(`s.ExecutorRegistryURL` field needs to be added to `Server` struct alongside `CCBrokerURL`; if not present, plumb it via env `EXECUTOR_REGISTRY_URL` in `cmd/serve.go`.)

- [ ] **Step 4: Wire route**

```go
// inside authenticated routes group
r.Post("/api/agent-sessions/{sid}/control", s.handleAgentSessionControl)
```

- [ ] **Step 5: Verify tests pass**

Run: `go test ./internal/server/ -run TestControl -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/handler_tui_control.go internal/server/handler_tui_control_test.go internal/server/server.go cmd/serve.go
git commit -m "feat(server): /api/agent-sessions/{sid}/control with model/permission/compact/cost/agents dispatch"
```

---

## Task 18: agentserver — cancel + decision + executor-status proxy handlers

**Files:**
- Create: `internal/server/handler_tui_proxy.go`
- Create: `internal/server/handler_tui_proxy_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/server/handler_tui_proxy_test.go
func TestCancelTurn_ProxiesToCCBroker(t *testing.T) {
    var ccPath string
    cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ccPath = r.URL.Path
        w.WriteHeader(http.StatusAccepted)
    }))
    defer cc.Close()
    s, _, _ := newTestServerWithFakes(t)
    s.CCBrokerURL = cc.URL
    sid := "cse_x"; tid := "trn_x"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1", CreatorUserID: "u",
    })
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/turns/"+tid+"/cancel", "{}")
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusAccepted {
        t.Errorf("status %d", rr.Code)
    }
    if !strings.Contains(ccPath, "/api/sessions/"+sid+"/turns/"+tid+"/cancel") {
        t.Errorf("cc-broker hit %q", ccPath)
    }
}

func TestDecidePermission_ProxiesAndForwardsResponderID(t *testing.T) {
    var receivedBody map[string]string
    cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        json.NewDecoder(r.Body).Decode(&receivedBody)
        w.WriteHeader(200)
    }))
    defer cc.Close()
    s, _, _ := newTestServerWithFakes(t)
    s.CCBrokerURL = cc.URL
    sid := "cse_d"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1", CreatorUserID: "u",
    })
    body := `{"decision":"allow","scope":"once","responder_executor_id":"exe_a"}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/permissions/perm_p1", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != 200 { t.Fatalf("status %d", rr.Code) }
    if receivedBody["verdict"] != "allow" || receivedBody["scope"] != "once" {
        t.Errorf("body=%v", receivedBody)
    }
    if receivedBody["by"] != "exe_a" {
        t.Errorf("by=%q want exe_a", receivedBody["by"])
    }
}

func TestExecutorStatus_ProxiesToRegistry(t *testing.T) {
    reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        w.Write([]byte(`{"executor_id":"exe_a","status":"online"}`))
    }))
    defer reg.Close()
    s, _, _ := newTestServerWithFakes(t)
    s.ExecutorRegistryURL = reg.URL
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "GET", "/api/executors/exe_a/status", "")
    s.Router().ServeHTTP(rr, req)
    if rr.Code != 200 { t.Fatalf("status %d", rr.Code) }
    if !strings.Contains(rr.Body.String(), `"status":"online"`) {
        t.Errorf("body=%s", rr.Body)
    }
}
```

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/server/ -run "TestCancelTurn|TestDecidePermission|TestExecutorStatus" -v`
Expected: FAIL.

- [ ] **Step 3: Implement proxies**

```go
// internal/server/handler_tui_proxy.go
package server

import (
    "bytes"
    "context"
    "encoding/json"
    "io"
    "net/http"
    "time"

    "github.com/go-chi/chi/v5"
)

func (s *Server) handleCancelTurn(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    tid := chi.URLParam(r, "tid")
    ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
    defer cancel()
    url := s.CCBrokerURL + "/api/sessions/" + sid + "/turns/" + tid + "/cancel"
    rq, _ := http.NewRequestWithContext(ctx, "POST", url, nil)
    resp, err := http.DefaultClient.Do(rq)
    if err != nil {
        writeAPIErr(w, 502, "upstream", "cc-broker unreachable")
        return
    }
    resp.Body.Close()
    w.WriteHeader(http.StatusAccepted)
    w.Write([]byte(`{"cancelled":true}`))
}

type decisionReq struct {
    Decision             string `json:"decision"`
    Scope                string `json:"scope"`
    ResponderExecutorID  string `json:"responder_executor_id"`
}

func (s *Server) handlePermissionDecision(w http.ResponseWriter, r *http.Request) {
    sid := chi.URLParam(r, "sid")
    pid := chi.URLParam(r, "pid")
    var req decisionReq
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeAPIErr(w, 400, "invalid", "invalid body")
        return
    }
    sess, err := s.DB.GetAgentSession(sid)
    if err != nil || sess == nil {
        writeAPIErr(w, 404, "not_found", "session not found")
        return
    }
    if sess.PermissionResponder == nil || *sess.PermissionResponder != req.ResponderExecutorID {
        writeAPIErr(w, 403, "forbidden", "not the current permission_responder")
        return
    }
    body, _ := json.Marshal(map[string]string{
        "verdict": req.Decision,
        "scope":   req.Scope,
        "by":      req.ResponderExecutorID,
    })
    ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
    defer cancel()
    url := s.CCBrokerURL + "/api/sessions/" + sid + "/permissions/" + pid + "/decide"
    rq, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
    rq.Header.Set("Content-Type", "application/json")
    resp, err := http.DefaultClient.Do(rq)
    if err != nil {
        writeAPIErr(w, 502, "upstream", "cc-broker unreachable")
        return
    }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusConflict {
        writeAPIErr(w, http.StatusConflict, "already_resolved", "permission already resolved")
        return
    }
    if resp.StatusCode != 200 {
        bb, _ := io.ReadAll(resp.Body)
        writeAPIErr(w, http.StatusBadGateway, "upstream", string(bb))
        return
    }
    json.NewEncoder(w).Encode(map[string]any{"accepted": true, "applied_at": time.Now().UTC()})
}

func (s *Server) handleExecutorStatus(w http.ResponseWriter, r *http.Request) {
    id := chi.URLParam(r, "id")
    url := s.ExecutorRegistryURL + "/api/executors/" + id
    ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
    defer cancel()
    rq, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
    resp, err := http.DefaultClient.Do(rq)
    if err != nil {
        writeAPIErr(w, 502, "upstream", "executor-registry unreachable")
        return
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(resp.StatusCode)
    w.Write(body)
}
```

- [ ] **Step 4: Wire routes**

```go
// inside authenticated routes group
r.Post("/api/agent-sessions/{sid}/turns/{tid}/cancel", s.handleCancelTurn)
r.Post("/api/agent-sessions/{sid}/permissions/{pid}", s.handlePermissionDecision)
r.Get ("/api/executors/{id}/status", s.handleExecutorStatus)
```

- [ ] **Step 5: Verify tests pass**

Run: `go test ./internal/server/ -run "TestCancelTurn|TestDecidePermission|TestExecutorStatus" -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/handler_tui_proxy.go internal/server/handler_tui_proxy_test.go internal/server/server.go
git commit -m "feat(server): cancel/decision/executor-status proxy handlers"
```

---

## Task 19: agentserver — internal /turn-finished callback handler

**Files:**
- Create: `internal/server/handler_tui_internal.go`
- Create: `internal/server/handler_tui_internal_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/server/handler_tui_internal_test.go
func TestTurnFinished_ClearsActiveTurnID(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_tf"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1", CreatorUserID: "u",
    })
    _, _ = s.DB.ClaimActiveTurn(context.Background(), sid, "trn_xyz")

    body := `{"session_id":"`+sid+`","turn_id":"trn_xyz"}`
    rr := httptest.NewRecorder()
    req := httptest.NewRequest("POST", "/internal/sessions/"+sid+"/turn-finished",
        strings.NewReader(body))
    if secret := os.Getenv("INTERNAL_API_SECRET"); secret != "" {
        req.Header.Set("X-Internal-Secret", secret)
    }
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusOK {
        t.Errorf("status %d", rr.Code)
    }
    cur, _ := s.DB.GetActiveTurn(context.Background(), sid)
    if cur != "" {
        t.Errorf("active_turn_id=%q want empty", cur)
    }
}

func TestTurnFinished_StaleTurnIDNoOp(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_stale"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1", CreatorUserID: "u",
    })
    _, _ = s.DB.ClaimActiveTurn(context.Background(), sid, "trn_current")

    // late callback for an old turn
    body := `{"session_id":"`+sid+`","turn_id":"trn_OLD"}`
    rr := httptest.NewRecorder()
    req := httptest.NewRequest("POST", "/internal/sessions/"+sid+"/turn-finished",
        strings.NewReader(body))
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusOK { t.Errorf("status %d", rr.Code) }
    cur, _ := s.DB.GetActiveTurn(context.Background(), sid)
    if cur != "trn_current" {
        t.Errorf("stale callback should NOT clear current; got %q", cur)
    }
}
```

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/server/ -run TestTurnFinished -v`
Expected: FAIL.

- [ ] **Step 3: Implement handler + auth gate**

```go
// internal/server/handler_tui_internal.go
package server

import (
    "encoding/json"
    "net/http"
    "os"

    "github.com/go-chi/chi/v5"
)

func (s *Server) handleTurnFinished(w http.ResponseWriter, r *http.Request) {
    if secret := os.Getenv("INTERNAL_API_SECRET"); secret != "" {
        if r.Header.Get("X-Internal-Secret") != secret {
            http.Error(w, "forbidden", http.StatusForbidden)
            return
        }
    }
    sid := chi.URLParam(r, "sid")
    var body struct {
        SessionID string `json:"session_id"`
        TurnID    string `json:"turn_id"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, "invalid body", http.StatusBadRequest)
        return
    }
    if body.SessionID != sid {
        http.Error(w, "session_id mismatch", http.StatusBadRequest)
        return
    }
    // ClearActiveTurn is idempotent + only clears when current matches (Task 3 Step 5)
    _ = s.DB.ClearActiveTurn(r.Context(), sid, body.TurnID)
    w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 4: Wire route — outside auth middleware (called by cc-broker)**

```go
// internal/server/server.go — outside the authenticated user routes, at top-level
r.Post("/internal/sessions/{sid}/turn-finished", s.handleTurnFinished)
```

- [ ] **Step 5: Verify tests pass**

Run: `go test ./internal/server/ -run TestTurnFinished -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/handler_tui_internal.go internal/server/handler_tui_internal_test.go internal/server/server.go
git commit -m "feat(server): /internal/sessions/{sid}/turn-finished callback for active_turn_id clear"
```

---

## Task 20: agentserver — leak worker (active_turn_id + responder TTL)

**Files:**
- Create: `internal/server/leak_worker.go`
- Create: `internal/server/leak_worker_test.go`
- Modify: `internal/server/server.go` (start the worker)

- [ ] **Step 1: Write failing test**

```go
// internal/server/leak_worker_test.go
func TestLeakWorker_ClearsStaleActiveTurn(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_leak"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1", CreatorUserID: "u",
    })
    _, _ = s.DB.ClaimActiveTurn(context.Background(), sid, "trn_dead")
    // Force updated_at to far past
    _, _ = s.DB.Exec(`UPDATE agent_sessions SET updated_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, sid)

    // cc-broker reports no active turn
    cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(`{"turn_id":null}`))
    }))
    defer cc.Close()
    s.CCBrokerURL = cc.URL

    lw := NewLeakWorker(s, LeakWorkerConfig{
        StaleTurnAfter: 5 * time.Minute,
        ResponderTTL:   90 * time.Second,
    })
    lw.RunOnce(context.Background())

    cur, _ := s.DB.GetActiveTurn(context.Background(), sid)
    if cur != "" {
        t.Errorf("expected leak worker to clear stale turn; still %q", cur)
    }
}

func TestLeakWorker_DoesNotClearActiveTurnIfCCBrokerStillReports(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_live"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1", CreatorUserID: "u",
    })
    _, _ = s.DB.ClaimActiveTurn(context.Background(), sid, "trn_alive")
    _, _ = s.DB.Exec(`UPDATE agent_sessions SET updated_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, sid)

    cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(`{"turn_id":"trn_alive"}`))
    }))
    defer cc.Close()
    s.CCBrokerURL = cc.URL

    lw := NewLeakWorker(s, LeakWorkerConfig{
        StaleTurnAfter: 5 * time.Minute,
        ResponderTTL:   90 * time.Second,
    })
    lw.RunOnce(context.Background())

    cur, _ := s.DB.GetActiveTurn(context.Background(), sid)
    if cur != "trn_alive" {
        t.Errorf("active turn cleared incorrectly; got %q", cur)
    }
}

func TestLeakWorker_ClearsStaleResponder(t *testing.T) {
    s, _, _ := newTestServerWithFakes(t)
    sid := "cse_resp"
    _ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
        ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:e:1", CreatorUserID: "u",
    })
    _, _ = s.DB.AttachResponder(context.Background(), sid, "exe_ghost", true)
    _, _ = s.DB.Exec(`UPDATE agent_sessions SET responder_attached_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, sid)

    lw := NewLeakWorker(s, LeakWorkerConfig{
        StaleTurnAfter: 5 * time.Minute,
        ResponderTTL:   90 * time.Second,
    })
    lw.RunOnce(context.Background())

    sess, _ := s.DB.GetAgentSession(sid)
    if sess.PermissionResponder != nil {
        t.Errorf("expected responder cleared; got %v", sess.PermissionResponder)
    }
}
```

- [ ] **Step 2: Verify fail**

Run: `go test ./internal/server/ -run TestLeakWorker -v`
Expected: FAIL.

- [ ] **Step 3: Implement worker**

```go
// internal/server/leak_worker.go
package server

import (
    "context"
    "encoding/json"
    "log"
    "net/http"
    "time"
)

type LeakWorkerConfig struct {
    StaleTurnAfter time.Duration  // default 5m
    ResponderTTL   time.Duration  // default 90s
    Period         time.Duration  // default 1m
}

type LeakWorker struct {
    s   *Server
    cfg LeakWorkerConfig
}

func NewLeakWorker(s *Server, cfg LeakWorkerConfig) *LeakWorker {
    if cfg.StaleTurnAfter == 0 { cfg.StaleTurnAfter = 5 * time.Minute }
    if cfg.ResponderTTL == 0   { cfg.ResponderTTL = 90 * time.Second }
    if cfg.Period == 0         { cfg.Period = time.Minute }
    return &LeakWorker{s: s, cfg: cfg}
}

func (l *LeakWorker) Run(ctx context.Context) {
    t := time.NewTicker(l.cfg.Period)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            l.RunOnce(ctx)
        }
    }
}

func (l *LeakWorker) RunOnce(ctx context.Context) {
    l.cleanStaleActiveTurns(ctx)
    l.cleanStaleResponders(ctx)
}

func (l *LeakWorker) cleanStaleActiveTurns(ctx context.Context) {
    cutoff := time.Now().Add(-l.cfg.StaleTurnAfter)
    pairs, err := l.s.DB.ListStaleActiveTurns(ctx, cutoff)
    if err != nil {
        log.Printf("leak: list stale active turns: %v", err)
        return
    }
    for _, p := range pairs {
        // Ask cc-broker if this turn is still active
        url := l.s.CCBrokerURL + "/api/sessions/" + p.SessionID + "/turns/active"
        rq, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
        resp, err := http.DefaultClient.Do(rq)
        if err != nil {
            log.Printf("leak: cc-broker query: %v", err)
            continue
        }
        var body struct{ TurnID *string `json:"turn_id"` }
        json.NewDecoder(resp.Body).Decode(&body)
        resp.Body.Close()
        if body.TurnID == nil || *body.TurnID != p.TurnID {
            // cc-broker doesn't have this turn → safe to clear
            _ = l.s.DB.ClearActiveTurn(ctx, p.SessionID, p.TurnID)
            log.Printf("leak: cleared stale active_turn_id session=%s turn=%s",
                p.SessionID, p.TurnID)
        }
    }
}

func (l *LeakWorker) cleanStaleResponders(ctx context.Context) {
    cutoff := time.Now().Add(-l.cfg.ResponderTTL)
    ids, err := l.s.DB.ListStaleResponders(ctx, cutoff)
    if err != nil {
        log.Printf("leak: list stale responders: %v", err)
        return
    }
    for _, sid := range ids {
        if err := l.s.DB.ClearResponder(ctx, sid); err != nil {
            log.Printf("leak: clear responder %s: %v", sid, err)
            continue
        }
        log.Printf("leak: cleared stale responder for session=%s", sid)
        // Best-effort: emit permission_orphaned via SSE (so subscribers know)
        l.s.BridgeHandler.SSE.Publish(sid, &bridge.StreamClientEvent{
            Type: "permission_responder_lost",
            Data: []byte(`{"reason":"ttl_expired"}`),
        })
    }
}
```

(Add `import "github.com/agentserver/agentserver/internal/bridge"` if missing.)

- [ ] **Step 4: Start worker in cmd/serve.go**

```go
// in cmd/serve.go after server setup
lw := server.NewLeakWorker(srv, server.LeakWorkerConfig{})
go lw.Run(ctx)
```

- [ ] **Step 5: Verify tests pass**

Run: `go test ./internal/server/ -run TestLeakWorker -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/leak_worker.go internal/server/leak_worker_test.go cmd/serve.go
git commit -m "feat(server): leak worker clears stale active_turn_id and responder TTL"
```

---

## Task 21: end-to-end backend smoke (httptest stack: agentserver + cc-broker + executor-registry)

**Files:**
- Create: `internal/server/e2e_tui_test.go`

This task is the safety net before declaring Phase 1 done. Builds a minimal in-memory stack and exercises the full happy path.

- [ ] **Step 1: Write the e2e test**

```go
// internal/server/e2e_tui_test.go
//go:build integration

package server

import (
    "context"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "sync"
    "testing"
    "time"
)

func TestE2E_TUITurnFlow(t *testing.T) {
    // 1. Fake executor-registry that owns exe_a (owner=u_test, online).
    reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        if strings.HasSuffix(r.URL.Path, "/api/executors/exe_a") {
            w.Write([]byte(`{"executor_id":"exe_a","owner_user_id":"u_test","shared_to_workspace":false,"status":"online"}`))
            return
        }
        w.Write([]byte(`{"executors":[]}`))
    }))
    defer reg.Close()

    // 2. Fake cc-broker that streams: tool_use → permission_request → expects decide → tool_result → turn_done.
    var (
        decideMu  sync.Mutex
        decideRcv chan map[string]string = make(chan map[string]string, 1)
    )
    cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        switch {
        case strings.HasSuffix(r.URL.Path, "/api/turns"):
            w.Header().Set("Content-Type", "text/event-stream")
            f := w.(http.Flusher)
            fmt.Fprint(w, "event: tool_use\ndata: {\"tool\":\"remote_bash\",\"executor_id\":\"exe_a\"}\n\n")
            f.Flush()
            fmt.Fprint(w, "event: permission_request\ndata: {\"permission_id\":\"perm_e\"}\n\n")
            f.Flush()
            // wait up to 2s for decide
            select {
            case <-decideRcv:
            case <-time.After(2 * time.Second):
                t.Errorf("decide timeout")
            }
            fmt.Fprint(w, "event: tool_result\ndata: {\"output\":\"ok\"}\n\n")
            f.Flush()
            fmt.Fprint(w, "event: turn_done\ndata: {}\n\n")
            f.Flush()
        case strings.Contains(r.URL.Path, "/permissions/perm_e/decide"):
            var b map[string]string
            json.NewDecoder(r.Body).Decode(&b)
            decideMu.Lock()
            decideRcv <- b
            decideMu.Unlock()
            w.WriteHeader(200)
        }
    }))
    defer cc.Close()

    // 3. Start a real agentserver pointed at fakes, with a real DB.
    s, _, _ := newTestServerWithFakes(t)
    s.CCBrokerURL = cc.URL
    s.ExecutorRegistryURL = reg.URL

    // 4. Subscribe SSE
    sid := ""
    received := make(chan string, 16)
    var subWG sync.WaitGroup
    subWG.Add(1)
    inboundFired := make(chan struct{})

    go func() {
        defer subWG.Done()
        <-inboundFired
        rr := httptest.NewRecorder()
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        req := mustAuthRequest(t, "GET", "/api/agent-sessions/"+sid+"/events", "")
        req = req.WithContext(ctx)
        s.Router().ServeHTTP(rr, req)
        for _, line := range strings.Split(rr.Body.String(), "\n") {
            if strings.HasPrefix(line, "event: ") {
                received <- strings.TrimPrefix(line, "event: ")
            }
        }
        close(received)
    }()

    // 5. POST inbound
    body := `{"executor_id":"exe_a","text":"hi","permission_responder":true}`
    rr := httptest.NewRecorder()
    req := mustAuthRequest(t, "POST", "/api/workspaces/ws_test/tui/inbound", body)
    s.Router().ServeHTTP(rr, req)
    if rr.Code != http.StatusAccepted { t.Fatalf("inbound %d", rr.Code) }
    var resp map[string]any
    json.Unmarshal(rr.Body.Bytes(), &resp)
    sid = resp["session_id"].(string)
    close(inboundFired)

    // 6. Wait for permission_request, then POST decide
    deadline := time.After(3 * time.Second)
    var sawPerm bool
    for !sawPerm {
        select {
        case ev := <-received:
            if ev == "permission_request" { sawPerm = true }
        case <-deadline:
            t.Fatal("no permission_request observed")
        }
    }
    decBody := `{"decision":"allow","scope":"once","responder_executor_id":"exe_a"}`
    decRR := httptest.NewRecorder()
    decReq := mustAuthRequest(t, "POST", "/api/agent-sessions/"+sid+"/permissions/perm_e", decBody)
    s.Router().ServeHTTP(decRR, decReq)
    if decRR.Code != 200 { t.Errorf("decide %d", decRR.Code) }

    // 7. Drain remaining events; expect tool_result + turn_done
    needed := map[string]bool{"tool_result": false, "turn_done": false}
    deadline = time.After(3 * time.Second)
    for !needed["tool_result"] || !needed["turn_done"] {
        select {
        case ev, ok := <-received:
            if !ok { goto check }
            if _, want := needed[ev]; want { needed[ev] = true }
        case <-deadline:
            goto check
        }
    }
check:
    for k, v := range needed {
        if !v { t.Errorf("missing event %q", k) }
    }
}
```

Build tag `integration` keeps this opt-in (`go test -tags=integration ./internal/server/...`).

- [ ] **Step 2: Run e2e test**

Run: `go test -tags=integration ./internal/server/ -run TestE2E_TUITurnFlow -v -timeout 20s`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/server/e2e_tui_test.go
git commit -m "test(server): e2e TUI turn flow integration smoke (build tag: integration)"
```

---

## Task 22: cmd/serve.go — wire ExecutorRegistryURL and InternalAPISecret env

**Files:**
- Modify: `cmd/serve.go`
- Modify: `internal/server/server.go` (add fields if absent)

- [ ] **Step 1: Add fields to Server struct**

In `internal/server/server.go` add (if not present):
```go
type Server struct {
    // existing fields...
    ExecutorRegistryURL string
}
```

- [ ] **Step 2: Wire from cmd/serve.go**

```go
// cmd/serve.go — when building Server
srv := &server.Server{
    DB:                  db,
    Auth:                authComponent,
    BridgeHandler:       bridgeHandler,
    CCBrokerURL:         os.Getenv("CCBROKER_URL"),
    ExecutorRegistryURL: os.Getenv("EXECUTOR_REGISTRY_URL"),
    IMBridgeURL:         os.Getenv("IMBRIDGE_URL"),
    // ... other existing wiring ...
}

// also export INTERNAL_API_SECRET for the handler_tui_internal.go gate (it reads env directly)
```

- [ ] **Step 3: Update README env vars table**

In `README.md` "Environment Variables (Main Server)" table, add:
```
| `CCBROKER_URL` | URL of the cc-broker service | (required for TUI flow) |
| `EXECUTOR_REGISTRY_URL` | URL of the executor-registry service | (required for TUI flow) |
| `INTERNAL_API_SECRET` | Shared secret for internal endpoints (e.g., turn-finished) | (recommended) |
```

- [ ] **Step 4: Build verification**

Run: `go build ./...`
Expected: success.

- [ ] **Step 5: Run full test suite**

Run: `go test ./...`
Expected: all PASS (skip integration tests with `-short` if needed locally).

- [ ] **Step 6: Commit**

```bash
git add cmd/serve.go internal/server/server.go README.md
git commit -m "chore: wire ExecutorRegistryURL and INTERNAL_API_SECRET env in serve cmd"
```

---

## Task 23: cc-broker — wire AGENTSERVER_INTERNAL_URL env in deployment

**Files:**
- Modify: `cmd/cc-broker/main.go` (or wherever cc-broker reads config)
- Modify: `Dockerfile.cc-broker` (env documentation only)

- [ ] **Step 1: Verify cc-broker config loads new keys**

Run: `grep -n "AgentserverInternalURL\|AGENTSERVER_INTERNAL_URL" internal/ccbroker/config.go`
Expected: present (added in Task 13). If reading from env requires explicit code, ensure `os.Getenv("AGENTSERVER_INTERNAL_URL")` is called in `LoadConfig()`.

- [ ] **Step 2: Update Dockerfile env documentation**

In `Dockerfile.cc-broker`:
```dockerfile
# TUI / permission gate integration:
#   AGENTSERVER_INTERNAL_URL — URL of agentserver for turn-finished callback
#   INTERNAL_API_SECRET      — shared secret for X-Internal-Secret header
```

- [ ] **Step 3: Verify cc-broker still builds and starts**

Run: `go build ./cmd/cc-broker/...`
Run: `./cc-broker --help` (or equivalent)
Expected: no errors; new env vars listed in any --help output if applicable.

- [ ] **Step 4: Commit**

```bash
git add cmd/cc-broker/main.go Dockerfile.cc-broker
git commit -m "chore(ccbroker): document AGENTSERVER_INTERNAL_URL and INTERNAL_API_SECRET env"
```

---

## Final Verification

- [ ] **Run all unit tests**: `go test ./...`
- [ ] **Run integration smoke**: `go test -tags=integration ./internal/server/ -run TestE2E_TUITurnFlow -v`
- [ ] **Build all binaries**: `make build`
- [ ] **Confirm no regressions in IM flow**: `go test ./internal/ccbroker/ -run "Regression|IM" -v`

