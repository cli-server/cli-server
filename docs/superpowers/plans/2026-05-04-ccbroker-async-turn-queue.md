# ccbroker async per-session turn queue — Implementation Plan (PR 1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace cc-broker's `turnLock` blocking model with a DB-backed FIFO queue + per-session worker goroutine, with crash recovery and cancellable queued/running turns. Existing `POST /api/turns` keeps the same SSE-streaming contract via a sync wrapper around the new enqueue path.

**Architecture:** Producer (HTTP) inserts an `agent_turns` row in state `queued`, an `agent_session_events` row for the user message tagged with `turn_id`, and signals a per-session `sessionWorker` via `workerRegistry.Notify`. The worker loop calls `PickNextPending`, runs the heavy path (`workspaceSetup → wstoken → runner.Run → pump SDK msgs → Teardown → MarkDone/Cancelled/Failed`), and broadcasts each event with `TurnID` populated. The HTTP handler subscribes to the SSE broker, filters by its own `turn_id`, and returns when it sees a terminal event for that turn (or the client disconnects). On cc-broker startup, `recoverPendingTurns` resets stale `running` rows back to `queued` and notifies one worker per session that has pending work.

**Tech Stack:** Go (chi router, pgx via `database/sql` + lib/pq), Postgres migrations embedded via `embed.FS`, Anthropic Agent SDK, existing in-process `SSEBroker`/`activeTurnRegistry`/`compactQueue`.

**Spec:** `docs/superpowers/specs/2026-05-04-ccbroker-async-turn-queue-design.md`

**Scope notes (deviations from spec to match codebase conventions, intentional):**
- Migration is `internal/ccbroker/migrations/002_agent_turns.sql`, not `internal/db/migrations/022_*.sql`. The spec drafts pre-date verification; the actual existing migration lives in the ccbroker package at version `001`.
- The `turn_id` column is added to `agent_session_events` (the actual table name), not `agent_events`.
- `turnStore` methods are added to the existing `*Store` in `internal/ccbroker/store.go` rather than a separate `internal/db/` package — this matches how `CreateSession`, `InsertEvents`, etc. are organized today.
- `runnerRun`, `workspaceSetup`, `workspaceTeardown` are already package-level seams (see `handler_turns.go:19-25`); the worker reuses them rather than introducing new function-typed deps.

---

## File structure

**New files:**
- `internal/ccbroker/migrations/002_agent_turns.sql` — schema for queue + turn_id
- `internal/ccbroker/agent_turns.go` — `AgentTurn` struct + `*Store` methods for queue ops
- `internal/ccbroker/agent_turns_test.go` — table-driven tests using sqlmock OR a build-tag'd Postgres test (skipped by default if no DB)
- `internal/ccbroker/session_worker.go` — `sessionWorker` struct + `run` loop + `execute` + `failTurn`
- `internal/ccbroker/session_worker_test.go` — worker behavior with stubbed deps
- `internal/ccbroker/worker_registry.go` — `workerRegistry` struct + `Notify`/`Shutdown`/`onIdleExit`
- `internal/ccbroker/worker_registry_test.go` — registry behavior
- `internal/ccbroker/recovery.go` — `recoverPendingTurns` Server method
- `internal/ccbroker/recovery_test.go` — recovery against fake store
- `internal/ccbroker/handler_turn_events.go` — `GET /api/turns/{tid}/events`
- `internal/ccbroker/handler_turn_events_test.go` — catch-up + tail tests
- `internal/ccbroker/handler_session_turns.go` — `GET /api/sessions/{sid}/turns`
- `internal/ccbroker/handler_session_turns_test.go` — list turns tests

**Modified files:**
- `internal/ccbroker/store.go` — extend `storer` interface; add `InsertEventsWithTurn`; rewrite `InsertEvents` to delegate
- `internal/ccbroker/models.go` — add `TurnID string` to `StreamClientEvent`; add `EventInput.TurnID` if needed; add `AgentTurn` type (or live in agent_turns.go)
- `internal/ccbroker/server.go` — drop `turnLock`, add `workerRegistry`, add `Start(ctx)`, add `Shutdown(ctx)`, register new routes
- `internal/ccbroker/handler_turns.go` — replace lock+execute with enqueue + Notify + SSE filter loop
- `internal/ccbroker/handler_turns_test.go` — update fakeStore to satisfy extended interface, drop turnLock assertions
- `internal/ccbroker/handler_tui_routes.go` — `handleCancelTurn` becomes state-aware (queued → MarkCancelled, running → activeTurns.Cancel, terminal → 410)
- `internal/ccbroker/handler_tui_routes_test.go` — coverage for queued + terminal cancel branches
- `internal/ccbroker/turn_lock.go` — **deleted** at the end (only after handler refactor lands)

---

## Task ordering rationale

1–3 build the **storage layer** (schema → Go wrappers → tests). Pure DB work, no goroutines yet.
4–5 build the **worker mechanism** (single worker → registry). Depends on 1–3 for `PickNextPending`/`MarkRunning`/etc.
6 builds **recovery** — Server startup hook that uses 1–5.
7 extends **events** with `turn_id` (SSE + DB column wiring) — the worker needs this to tag broadcasts; we defer until after 1–5 because `InsertEventsWithTurn` is consumed only in the worker `execute` path.
8 **refactors handler_turns.go** to use enqueue + Notify + SSE filter. This is the cutover step where `turnLock` is removed.
9 updates **cancel** for queued state.
10 adds new **read-only endpoints** (`/turns/{tid}/events`, `/sessions/{sid}/turns`).
11 wires everything in `server.go` (final `Start`/`Shutdown` plumbing, route registration), runs the full test suite, deletes `turn_lock.go`.

---

## Task 1: Migration 002 — `agent_turns` table + `turn_id` column

**Files:**
- Create: `internal/ccbroker/migrations/002_agent_turns.sql`
- Test: `internal/ccbroker/agent_turns_test.go` (created; only verifies migration applies cleanly via `NewStore` if DB available — otherwise the test is skipped)

- [ ] **Step 1: Write the migration**

Create `internal/ccbroker/migrations/002_agent_turns.sql`:

```sql
CREATE TABLE IF NOT EXISTS agent_turns (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('queued','running','done','cancelled','failed')),
    user_event_id TEXT NOT NULL,
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
    im_channel_id TEXT,
    im_user_id    TEXT,
    user_message  TEXT NOT NULL,
    error_msg     TEXT,
    enqueued_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_agent_turns_pending ON agent_turns(session_id, enqueued_at)
    WHERE state IN ('queued','running');
CREATE INDEX IF NOT EXISTS idx_agent_turns_session ON agent_turns(session_id, enqueued_at DESC);

ALTER TABLE agent_session_events ADD COLUMN IF NOT EXISTS turn_id TEXT;
CREATE INDEX IF NOT EXISTS idx_agent_session_events_turn
    ON agent_session_events(turn_id) WHERE turn_id IS NOT NULL;
```

Note: `user_message` is included in the row so the worker can re-fetch it on recovery without joining `agent_session_events`. Decision matches the spec's intent (worker picks turn from DB and runs it independently).

`agent_sessions` does NOT use `ON DELETE CASCADE` here because the session table doesn't have an FK guarantee in this schema; we stay loose to match the existing `agent_session_events.session_id` (also unconstrained).

- [ ] **Step 2: Verify migration applies**

If a Postgres dev DB is available, run:

```bash
go test ./internal/ccbroker/ -run TestMigrationApplies -v
```

(Test is added in Task 2 along with the agent_turns Go wrapper. If no DB, the test is `t.Skip`ped.)

- [ ] **Step 3: Commit**

```bash
git add internal/ccbroker/migrations/002_agent_turns.sql
git commit -m "feat(ccbroker): add migration 002 for agent_turns queue"
```

---

## Task 2: `AgentTurn` model + `*Store` methods

**Files:**
- Create: `internal/ccbroker/agent_turns.go`
- Create: `internal/ccbroker/agent_turns_test.go`
- Modify: `internal/ccbroker/store.go` (extend `storer` interface)

- [ ] **Step 1: Write the failing test for storer interface compatibility**

Create `internal/ccbroker/agent_turns_test.go` with a compile-time assertion that the new methods exist on `*Store`:

```go
package ccbroker

import "testing"

func TestStoreImplementsTurnQueueOps(t *testing.T) {
	// Compile-time: *Store must satisfy the extended storer interface that
	// includes turn-queue ops. If this test file compiles, the contract holds.
	var _ storer = (*Store)(nil)
}
```

Run: `go build ./internal/ccbroker/` → expected FAIL: `*Store does not implement storer (missing methods)`.

- [ ] **Step 2: Extend the `storer` interface**

In `internal/ccbroker/store.go`, replace the `storer` interface block (lines 23-28) with:

```go
type storer interface {
	GetSession(ctx context.Context, id string) (*Session, error)
	CreateSession(ctx context.Context, id, workspaceID, title, source string, externalID *string) error
	GetSessionEpoch(ctx context.Context, sessionID string) (int, error)
	InsertEvents(ctx context.Context, sessionID string, epoch int, events []EventInput) ([]InsertedEvent, error)
	InsertEventsWithTurn(ctx context.Context, sessionID string, epoch int, turnID string, events []EventInput) ([]InsertedEvent, error)

	// Turn queue ops
	EnqueueTurn(ctx context.Context, t AgentTurn) error
	PickNextPending(ctx context.Context, sessionID string) (*AgentTurn, error)
	MarkTurnRunning(ctx context.Context, turnID string) error
	MarkTurnDone(ctx context.Context, turnID string) error
	MarkTurnCancelled(ctx context.Context, turnID string) error
	MarkTurnFailed(ctx context.Context, turnID, errMsg string) error
	GetTurn(ctx context.Context, turnID string) (*AgentTurn, error)
	ListSessionsWithPending(ctx context.Context) ([]string, error)
	ListSessionTurns(ctx context.Context, sessionID string, limit int) ([]AgentTurn, error)
	ResetRunningToQueued(ctx context.Context) (int, error)
	CountPending(ctx context.Context, sessionID string) (int, error)
}
```

- [ ] **Step 3: Write `agent_turns.go` with the model + methods**

Create `internal/ccbroker/agent_turns.go`:

```go
package ccbroker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AgentTurn mirrors a row in the agent_turns table.
type AgentTurn struct {
	ID           string
	SessionID    string
	WorkspaceID  string
	State        string // queued|running|done|cancelled|failed
	UserEventID  string
	UserMessage  string
	Metadata     json.RawMessage
	IMChannelID  sql.NullString
	IMUserID     sql.NullString
	ErrorMsg     sql.NullString
	EnqueuedAt   time.Time
	StartedAt    sql.NullTime
	FinishedAt   sql.NullTime
}

func (s *Store) EnqueueTurn(ctx context.Context, t AgentTurn) error {
	meta := t.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	_, err := s.ExecContext(ctx,
		`INSERT INTO agent_turns
		   (id, session_id, workspace_id, state, user_event_id, user_message,
		    metadata, im_channel_id, im_user_id)
		 VALUES ($1, $2, $3, 'queued', $4, $5, $6, $7, $8)`,
		t.ID, t.SessionID, t.WorkspaceID, t.UserEventID, t.UserMessage,
		meta, nullableString(t.IMChannelID), nullableString(t.IMUserID),
	)
	if err != nil {
		return fmt.Errorf("enqueue turn: %w", err)
	}
	return nil
}

func (s *Store) PickNextPending(ctx context.Context, sessionID string) (*AgentTurn, error) {
	row := s.QueryRowContext(ctx,
		`SELECT id, session_id, workspace_id, state, user_event_id, user_message,
		        metadata, im_channel_id, im_user_id, error_msg,
		        enqueued_at, started_at, finished_at
		 FROM agent_turns
		 WHERE session_id = $1 AND state IN ('queued','running')
		 ORDER BY enqueued_at ASC
		 LIMIT 1`, sessionID)
	t := &AgentTurn{}
	err := row.Scan(&t.ID, &t.SessionID, &t.WorkspaceID, &t.State, &t.UserEventID, &t.UserMessage,
		&t.Metadata, &t.IMChannelID, &t.IMUserID, &t.ErrorMsg,
		&t.EnqueuedAt, &t.StartedAt, &t.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick next pending: %w", err)
	}
	return t, nil
}

func (s *Store) MarkTurnRunning(ctx context.Context, turnID string) error {
	res, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='running', started_at=NOW()
		 WHERE id=$1 AND state='queued'`, turnID)
	if err != nil {
		return fmt.Errorf("mark turn running: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either already-running (recovery path) or terminal — caller decides.
		return nil
	}
	return nil
}

func (s *Store) MarkTurnDone(ctx context.Context, turnID string) error {
	_, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='done', finished_at=NOW()
		 WHERE id=$1 AND state IN ('running','queued')`, turnID)
	if err != nil {
		return fmt.Errorf("mark turn done: %w", err)
	}
	return nil
}

func (s *Store) MarkTurnCancelled(ctx context.Context, turnID string) error {
	_, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='cancelled', finished_at=NOW()
		 WHERE id=$1 AND state IN ('queued','running')`, turnID)
	if err != nil {
		return fmt.Errorf("mark turn cancelled: %w", err)
	}
	return nil
}

func (s *Store) MarkTurnFailed(ctx context.Context, turnID, errMsg string) error {
	_, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='failed', finished_at=NOW(), error_msg=$2
		 WHERE id=$1 AND state IN ('queued','running')`, turnID, errMsg)
	if err != nil {
		return fmt.Errorf("mark turn failed: %w", err)
	}
	return nil
}

func (s *Store) GetTurn(ctx context.Context, turnID string) (*AgentTurn, error) {
	row := s.QueryRowContext(ctx,
		`SELECT id, session_id, workspace_id, state, user_event_id, user_message,
		        metadata, im_channel_id, im_user_id, error_msg,
		        enqueued_at, started_at, finished_at
		 FROM agent_turns WHERE id=$1`, turnID)
	t := &AgentTurn{}
	err := row.Scan(&t.ID, &t.SessionID, &t.WorkspaceID, &t.State, &t.UserEventID, &t.UserMessage,
		&t.Metadata, &t.IMChannelID, &t.IMUserID, &t.ErrorMsg,
		&t.EnqueuedAt, &t.StartedAt, &t.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get turn: %w", err)
	}
	return t, nil
}

func (s *Store) ListSessionsWithPending(ctx context.Context) ([]string, error) {
	rows, err := s.QueryContext(ctx,
		`SELECT DISTINCT session_id FROM agent_turns WHERE state IN ('queued','running')`)
	if err != nil {
		return nil, fmt.Errorf("list pending sessions: %w", err)
	}
	defer rows.Close()
	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("scan sid: %w", err)
		}
		sids = append(sids, sid)
	}
	return sids, rows.Err()
}

func (s *Store) ListSessionTurns(ctx context.Context, sessionID string, limit int) ([]AgentTurn, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.QueryContext(ctx,
		`SELECT id, session_id, workspace_id, state, user_event_id, user_message,
		        metadata, im_channel_id, im_user_id, error_msg,
		        enqueued_at, started_at, finished_at
		 FROM agent_turns WHERE session_id=$1
		 ORDER BY enqueued_at DESC LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list session turns: %w", err)
	}
	defer rows.Close()
	var out []AgentTurn
	for rows.Next() {
		var t AgentTurn
		if err := rows.Scan(&t.ID, &t.SessionID, &t.WorkspaceID, &t.State, &t.UserEventID, &t.UserMessage,
			&t.Metadata, &t.IMChannelID, &t.IMUserID, &t.ErrorMsg,
			&t.EnqueuedAt, &t.StartedAt, &t.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ResetRunningToQueued(ctx context.Context) (int, error) {
	res, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='queued', started_at=NULL
		 WHERE state='running'`)
	if err != nil {
		return 0, fmt.Errorf("reset running to queued: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) CountPending(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_turns
		 WHERE session_id=$1 AND state IN ('queued','running')`,
		sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// InsertEventsWithTurn is identical to InsertEvents but tags each row with turn_id.
func (s *Store) InsertEventsWithTurn(ctx context.Context, sessionID string, epoch int, turnID string, events []EventInput) ([]InsertedEvent, error) {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO agent_session_events
		   (session_id, event_id, event_type, source, epoch, payload, ephemeral, turn_id)
		 VALUES ($1, $2, 'client_event', 'worker', $3, $4, $5, $6)
		 ON CONFLICT (event_id) DO NOTHING
		 RETURNING id`)
	if err != nil {
		return nil, fmt.Errorf("prepare insert events with turn: %w", err)
	}
	defer stmt.Close()
	var inserted []InsertedEvent
	for _, e := range events {
		var seqNum int64
		var tid interface{} = turnID
		if turnID == "" {
			tid = nil
		}
		err := stmt.QueryRowContext(ctx, sessionID, e.EventID, epoch, e.Payload, e.Ephemeral, tid).Scan(&seqNum)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("insert event %s: %w", e.EventID, err)
		}
		inserted = append(inserted, InsertedEvent{SeqNum: seqNum, EventID: e.EventID})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit insert events with turn: %w", err)
	}
	return inserted, nil
}

func nullableString(v sql.NullString) interface{} {
	if !v.Valid {
		return nil
	}
	return v.String
}
```

- [ ] **Step 4: Update existing `InsertEvents` to delegate**

Replace the body of `InsertEvents` in `store.go` (lines 161-196) with:

```go
func (s *Store) InsertEvents(ctx context.Context, sessionID string, epoch int, events []EventInput) ([]InsertedEvent, error) {
	return s.InsertEventsWithTurn(ctx, sessionID, epoch, "", events)
}
```

- [ ] **Step 5: Build & confirm compile passes**

Run: `go build ./internal/ccbroker/`
Expected: PASS (no compile errors).

- [ ] **Step 6: Add migration-applies test (skipped without DB)**

Append to `agent_turns_test.go`:

```go
func TestMigrationApplies(t *testing.T) {
	url := getTestPostgresURL(t)
	if url == "" {
		t.Skip("no test postgres URL set; skipping")
	}
	st, err := NewStore(url)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer st.Close()

	// Round-trip a turn so the schema is exercised.
	sid := "sess_test_" + randomSuffix(t)
	wid := "ws_test"
	if err := st.CreateSession(context.Background(), sid, wid, "", "test", nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	turn := AgentTurn{
		ID: "trn_test_" + randomSuffix(t), SessionID: sid, WorkspaceID: wid,
		UserEventID: "evt_x", UserMessage: "hi",
	}
	if err := st.EnqueueTurn(context.Background(), turn); err != nil {
		t.Fatalf("EnqueueTurn: %v", err)
	}
	got, err := st.PickNextPending(context.Background(), sid)
	if err != nil {
		t.Fatalf("PickNextPending: %v", err)
	}
	if got == nil || got.ID != turn.ID || got.State != "queued" {
		t.Fatalf("unexpected pick: %+v", got)
	}
}

func getTestPostgresURL(t *testing.T) string {
	t.Helper()
	return os.Getenv("CCBROKER_TEST_POSTGRES_URL")
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}
```

Add the imports at the top: `context`, `crypto/rand`, `encoding/hex`, `os`.

- [ ] **Step 7: Run go vet and the unit tests**

```bash
go vet ./internal/ccbroker/
go test ./internal/ccbroker/ -run TestStoreImplementsTurnQueueOps -v
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/ccbroker/agent_turns.go internal/ccbroker/agent_turns_test.go internal/ccbroker/store.go
git commit -m "feat(ccbroker): add agent_turns store ops + InsertEventsWithTurn"
```

---

## Task 3: Update `fakeStore` in handler_turns_test.go to satisfy extended interface

**Files:**
- Modify: `internal/ccbroker/handler_turns_test.go`

The `fakeStore` (lines 106-150) currently implements only the original 4 methods. Adding the queue methods to the interface broke compilation of the test file. Make it whole.

- [ ] **Step 1: Verify the build is broken**

```bash
go test -c ./internal/ccbroker/ -o /dev/null
```

Expected: FAIL — `*fakeStore does not implement storer`.

- [ ] **Step 2: Extend `fakeStore` with all queue methods**

In `handler_turns_test.go`, find the `fakeStore` struct and append fields + methods. Replace the struct block:

```go
type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]*Session

	// Queue state
	turns      map[string]*AgentTurn  // by turn_id
	turnOrder  map[string][]string    // session_id → ordered turn_ids
	resetCount int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions:  make(map[string]*Session),
		turns:     make(map[string]*AgentTurn),
		turnOrder: make(map[string][]string),
	}
}
```

Append the new methods (after the existing `InsertEvents`):

```go
func (f *fakeStore) InsertEventsWithTurn(_ context.Context, _ string, _ int, _ string, events []EventInput) ([]InsertedEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InsertedEvent, 0, len(events))
	for _, e := range events {
		out = append(out, InsertedEvent{SeqNum: int64(len(out) + 1), EventID: e.EventID})
	}
	return out, nil
}

func (f *fakeStore) EnqueueTurn(_ context.Context, t AgentTurn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := t
	cp.State = "queued"
	cp.EnqueuedAt = time.Now()
	f.turns[t.ID] = &cp
	f.turnOrder[t.SessionID] = append(f.turnOrder[t.SessionID], t.ID)
	return nil
}

func (f *fakeStore) PickNextPending(_ context.Context, sessionID string) (*AgentTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tid := range f.turnOrder[sessionID] {
		t := f.turns[tid]
		if t.State == "queued" || t.State == "running" {
			cp := *t
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) MarkTurnRunning(_ context.Context, turnID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.turns[turnID]; ok && t.State == "queued" {
		t.State = "running"
	}
	return nil
}

func (f *fakeStore) MarkTurnDone(_ context.Context, turnID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.turns[turnID]; ok {
		t.State = "done"
	}
	return nil
}

func (f *fakeStore) MarkTurnCancelled(_ context.Context, turnID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.turns[turnID]; ok {
		t.State = "cancelled"
	}
	return nil
}

func (f *fakeStore) MarkTurnFailed(_ context.Context, turnID, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.turns[turnID]; ok {
		t.State = "failed"
		t.ErrorMsg = sql.NullString{String: errMsg, Valid: true}
	}
	return nil
}

func (f *fakeStore) GetTurn(_ context.Context, turnID string) (*AgentTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.turns[turnID]
	if !ok {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (f *fakeStore) ListSessionsWithPending(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]struct{}{}
	var sids []string
	for _, t := range f.turns {
		if t.State == "queued" || t.State == "running" {
			if _, ok := seen[t.SessionID]; !ok {
				seen[t.SessionID] = struct{}{}
				sids = append(sids, t.SessionID)
			}
		}
	}
	return sids, nil
}

func (f *fakeStore) ListSessionTurns(_ context.Context, sessionID string, limit int) ([]AgentTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []AgentTurn{}
	ids := f.turnOrder[sessionID]
	for i := len(ids) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *f.turns[ids[i]])
	}
	return out, nil
}

func (f *fakeStore) ResetRunningToQueued(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.turns {
		if t.State == "running" {
			t.State = "queued"
			n++
		}
	}
	f.resetCount = n
	return n, nil
}

func (f *fakeStore) CountPending(_ context.Context, sessionID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tid := range f.turnOrder[sessionID] {
		if t := f.turns[tid]; t.State == "queued" || t.State == "running" {
			n++
		}
	}
	return n, nil
}
```

Add imports if missing: `database/sql`.

- [ ] **Step 3: Verify build passes**

```bash
go test -c ./internal/ccbroker/ -o /dev/null
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/ccbroker/handler_turns_test.go
git commit -m "test(ccbroker): extend fakeStore for turn queue ops"
```

---

## Task 4: `sessionWorker` skeleton + `run` loop (no `execute` body yet)

**Files:**
- Create: `internal/ccbroker/session_worker.go`
- Create: `internal/ccbroker/session_worker_test.go`

- [ ] **Step 1: Write the failing test for the worker run loop**

Create `internal/ccbroker/session_worker_test.go`:

```go
package ccbroker

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestWorkerProcessesSingleTurn asserts that, given one queued turn, the
// worker calls execute exactly once, then sleeps until idle timeout.
func TestWorkerProcessesSingleTurn(t *testing.T) {
	store := newFakeStore()
	sid := "sess_x"
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: "ws_y"}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_1", SessionID: sid, WorkspaceID: "ws_y", UserEventID: "evt_a", UserMessage: "hi",
	})

	var executed atomic.Int32
	var idleExited atomic.Int32
	w := &sessionWorker{
		sessionID: sid,
		wake:      make(chan struct{}, 1),
		quit:      make(chan struct{}),
		idleAfter: 50 * time.Millisecond,
		deps: workerDeps{
			store:  store,
			logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		},
		executeFn: func(_ context.Context, _ *AgentTurn) {
			executed.Add(1)
		},
		onIdleExit: func(_ string) { idleExited.Add(1) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("worker did not idle-exit in time")
	}
	if executed.Load() != 1 {
		t.Fatalf("expected execute once, got %d", executed.Load())
	}
	if idleExited.Load() != 1 {
		t.Fatalf("expected onIdleExit once, got %d", idleExited.Load())
	}
}
```

Run: `go test ./internal/ccbroker/ -run TestWorkerProcessesSingleTurn -v`
Expected: FAIL — `sessionWorker` undefined.

- [ ] **Step 2: Write `session_worker.go` skeleton**

Create `internal/ccbroker/session_worker.go`:

```go
package ccbroker

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/runner"
	"github.com/agentserver/agentserver/internal/ccbroker/tools"
	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

const defaultWorkerIdleTimeout = 5 * time.Minute

// workerDeps bundles everything a sessionWorker needs to run a turn.
// Function-typed callbacks (workspaceSetup, runnerRun, callTurnFinished)
// match the existing package seams in handler_turns.go, so tests can stub
// without holding a real Server.
type workerDeps struct {
	store             storer
	s3                *workspace.S3Store
	wstoken           func(ctx context.Context, workspaceID string) (string, error)
	sse               *SSEBroker
	activeTurns       *activeTurnRegistry
	compactQueue      *compactQueue
	gate              *tools.Gate
	logger            *slog.Logger
	config            Config
	httpClient        *http.Client
	workspaceSetup    func(ctx context.Context, wid, sid string, s3 *workspace.S3Store) (*workspace.Workspace, error)
	workspaceTeardown func(ctx context.Context, ws *workspace.Workspace, s3 *workspace.S3Store) error
	runnerRun         func(ctx context.Context, ws *workspace.Workspace, sid, msg string, cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error)
	callTurnFinished  func(sid, tid string)
}

// sessionWorker drains the queue for one session_id. Each Notify wake-up
// causes it to PickNextPending in a loop until empty, then sleep until
// idleAfter elapses, at which point it removes itself from the registry.
type sessionWorker struct {
	sessionID  string
	wake       chan struct{} // buffer=1
	quit       chan struct{}
	idleAfter  time.Duration
	deps       workerDeps
	onIdleExit func(sessionID string)

	// executeFn is the heavy path; tests inject a stub. Production wiring
	// (Task 5) defaults this to (*sessionWorker).execute.
	executeFn func(ctx context.Context, t *AgentTurn)
}

func newSessionWorker(sessionID string, deps workerDeps, onIdleExit func(string)) *sessionWorker {
	w := &sessionWorker{
		sessionID:  sessionID,
		wake:       make(chan struct{}, 1),
		quit:       make(chan struct{}),
		idleAfter:  defaultWorkerIdleTimeout,
		deps:       deps,
		onIdleExit: onIdleExit,
	}
	w.executeFn = w.execute
	return w
}

func (w *sessionWorker) run(ctx context.Context) {
	idle := time.NewTimer(w.idleAfter)
	defer idle.Stop()
	for {
		turn, err := w.deps.store.PickNextPending(ctx, w.sessionID)
		if err != nil {
			w.deps.logger.Error("worker pick next failed",
				"session_id", w.sessionID, "error", err)
			select {
			case <-time.After(time.Second):
			case <-w.quit:
				return
			case <-ctx.Done():
				return
			}
			continue
		}
		if turn != nil {
			w.executeFn(ctx, turn)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(w.idleAfter)
			continue
		}
		select {
		case <-w.wake:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(w.idleAfter)
		case <-idle.C:
			if w.onIdleExit != nil {
				w.onIdleExit(w.sessionID)
			}
			return
		case <-w.quit:
			return
		case <-ctx.Done():
			return
		}
	}
}

// execute is the production heavy path; populated in Task 5.
func (w *sessionWorker) execute(ctx context.Context, t *AgentTurn) {
	// Implemented in Task 5.
}
```

- [ ] **Step 3: Run the test, confirm pass**

```bash
go test ./internal/ccbroker/ -run TestWorkerProcessesSingleTurn -v
```

Expected: PASS.

- [ ] **Step 4: Add wake-from-idle test**

Append to `session_worker_test.go`:

```go
func TestWorkerWakesFromIdle(t *testing.T) {
	store := newFakeStore()
	sid := "sess_w"

	var executed atomic.Int32
	w := &sessionWorker{
		sessionID: sid,
		wake:      make(chan struct{}, 1),
		quit:      make(chan struct{}),
		idleAfter: 5 * time.Second, // long; we'll wake via Notify
		deps: workerDeps{
			store:  store,
			logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		},
		executeFn: func(_ context.Context, _ *AgentTurn) { executed.Add(1) },
		onIdleExit: func(_ string) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.run(ctx)

	// Worker should be sleeping (no turns enqueued yet).
	time.Sleep(50 * time.Millisecond)
	if executed.Load() != 0 {
		t.Fatalf("worker should not have executed yet")
	}

	// Enqueue, wake, expect execute.
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_w", SessionID: sid, WorkspaceID: "ws", UserEventID: "evt", UserMessage: "hi",
	})
	w.wake <- struct{}{}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if executed.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if executed.Load() != 1 {
		t.Fatalf("worker did not execute after wake (got %d)", executed.Load())
	}
	close(w.quit)
}
```

Run: `go test ./internal/ccbroker/ -run TestWorkerWakesFromIdle -v`
Expected: PASS.

- [ ] **Step 5: Add quit test**

Append:

```go
func TestWorkerExitsOnQuit(t *testing.T) {
	w := &sessionWorker{
		sessionID: "sess_q",
		wake:      make(chan struct{}, 1),
		quit:      make(chan struct{}),
		idleAfter: 10 * time.Second,
		deps: workerDeps{
			store:  newFakeStore(),
			logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		},
		executeFn:  func(_ context.Context, _ *AgentTurn) {},
		onIdleExit: func(_ string) {},
	}
	done := make(chan struct{})
	go func() { w.run(context.Background()); close(done) }()
	close(w.quit)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("worker did not exit on quit")
	}
}
```

Run: `go test ./internal/ccbroker/ -run TestWorker -v`
Expected: ALL PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ccbroker/session_worker.go internal/ccbroker/session_worker_test.go
git commit -m "feat(ccbroker): add sessionWorker run loop (execute stubbed)"
```

---

## Task 5: `sessionWorker.execute` heavy path

**Files:**
- Modify: `internal/ccbroker/session_worker.go`
- Modify: `internal/ccbroker/session_worker_test.go`
- Modify: `internal/ccbroker/models.go` (add `TurnID` to `StreamClientEvent`)

- [ ] **Step 1: Add `TurnID` to `StreamClientEvent`**

In `internal/ccbroker/models.go`, replace the `StreamClientEvent` struct:

```go
type StreamClientEvent struct {
	EventID     string          `json:"event_id"`
	SequenceNum int64           `json:"sequence_num"`
	EventType   string          `json:"event_type"`
	Source      string          `json:"source"`
	TurnID      string          `json:"turn_id,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   string          `json:"created_at"`
}
```

- [ ] **Step 2: Write the failing test for execute happy-path**

In `session_worker_test.go`, add:

```go
func TestExecuteHappyPath(t *testing.T) {
	sid, wid, tid := "sess_e", "ws_e", "trn_e"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt_u", UserMessage: "hello",
	})

	sse := NewSSEBroker()
	sub := sse.Subscribe(sid)
	defer sse.Unsubscribe(sid, sub)

	teardownCalled := atomic.Int32{}
	deps := workerDeps{
		store:        store,
		s3:           nil,
		sse:          sse,
		activeTurns:  newActiveTurnRegistry(),
		compactQueue: newCompactQueue(),
		gate:         tools.NewGate(func(string, tools.Event) {}),
		logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient:   http.DefaultClient,
		wstoken: func(_ context.Context, _ string) (string, error) {
			return "tok", nil
		},
		workspaceSetup: func(_ context.Context, w, s string, _ *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{WorkspaceID: w, SessionID: s, TempDir: "/tmp/x"}, nil
		},
		workspaceTeardown: func(_ context.Context, _ *workspace.Workspace, _ *workspace.S3Store) error {
			teardownCalled.Add(1)
			return nil
		},
		runnerRun: func(_ context.Context, _ *workspace.Workspace, _, _ string, _ runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			ch := make(chan agentsdk.SDKMessage, 2)
			ch <- agentsdk.SDKMessage{Type: "assistant", Raw: json.RawMessage(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)}
			ch <- agentsdk.SDKMessage{Type: "result", Subtype: "success", Raw: json.RawMessage(`{"type":"result","subtype":"success","is_error":false}`)}
			close(ch)
			return ch, nil
		},
		callTurnFinished: func(_, _ string) {},
	}
	w := newSessionWorker(sid, deps, func(string) {})

	turn, _ := store.PickNextPending(context.Background(), sid)
	if turn == nil {
		t.Fatalf("expected pending turn")
	}
	w.execute(context.Background(), turn)

	if teardownCalled.Load() != 1 {
		t.Fatalf("teardown not called")
	}
	got, _ := store.GetTurn(context.Background(), tid)
	if got == nil || got.State != "done" {
		t.Fatalf("expected done, got %+v", got)
	}

	// Drain published events; expect at least one with our TurnID.
	gotTurnID := false
loop:
	for {
		select {
		case ev := <-sub.Ch:
			if ev.TurnID == tid {
				gotTurnID = true
			}
		default:
			break loop
		}
	}
	if !gotTurnID {
		t.Fatalf("no event tagged with TurnID")
	}
}
```

Add imports: `encoding/json`, `net/http`, plus the existing `runner`, `tools`, `workspace`, `agentsdk` already imported in handler_turns_test.go.

Run: `go test ./internal/ccbroker/ -run TestExecuteHappyPath -v`
Expected: FAIL — execute body is empty, state stays "queued".

- [ ] **Step 3: Implement `execute`**

In `session_worker.go`, replace the empty `execute` method:

```go
func (w *sessionWorker) execute(ctx context.Context, turn *AgentTurn) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.deps.activeTurns.Set(turn.SessionID, turn.ID, cancel)
	defer w.deps.activeTurns.Clear(turn.SessionID, turn.ID)
	defer w.deps.callTurnFinished(turn.SessionID, turn.ID)

	if err := w.deps.store.MarkTurnRunning(ctx, turn.ID); err != nil {
		w.deps.logger.Error("worker mark running failed",
			"session_id", turn.SessionID, "turn_id", turn.ID, "error", err)
		return
	}

	wsTok, err := w.deps.wstoken(ctx, turn.WorkspaceID)
	if err != nil {
		w.failTurn(ctx, turn, "workspace token: "+err.Error())
		return
	}

	ws, err := w.deps.workspaceSetup(ctx, turn.WorkspaceID, turn.SessionID, w.deps.s3)
	if err != nil {
		w.failTurn(ctx, turn, "workspace setup: "+err.Error())
		return
	}
	defer func() {
		_ = w.deps.workspaceTeardown(context.Background(), ws, w.deps.s3)
	}()

	// Honor compaction request queued via /compact since the previous turn.
	turnKind := ""
	if w.deps.compactQueue.Take(turn.SessionID) {
		turnKind = "compaction"
	}

	// Decode metadata for per-turn settings.
	var meta TurnMetadata
	if len(turn.Metadata) > 0 {
		_ = json.Unmarshal(turn.Metadata, &meta)
	}
	channelType := defaultStr(meta.ChannelType, "im")
	permMode := defaultStr(meta.PermissionMode, "bypass")

	tctx := &tools.Context{
		SessionID:              turn.SessionID,
		WorkspaceID:            turn.WorkspaceID,
		IMChannelID:            turn.IMChannelID.String,
		IMUserID:               turn.IMUserID.String,
		ExecutorRegistryURL:    w.deps.config.ExecutorRegistryURL,
		AgentserverURL:         w.deps.config.AgentserverURL,
		IMBridgeURL:            w.deps.config.IMBridgeURL,
		InternalAPISecret:      w.deps.config.IMBridgeSecret,
		Workspace:              ws,
		HTTP:                   w.deps.httpClient,
		ChannelType:            channelType,
		CreatorUserID:          meta.CreatorUserID,
		PermissionMode:         permMode,
		PreferredExecutorID:    meta.PreferredExecutorID,
		Gate:                   w.deps.gate,
		AgentserverInternalURL: w.deps.config.AgentserverInternalURL,
		CurrentTurnID:          turn.ID,
	}
	mcp := tools.BuildMcpServer(tctx)

	runCfg := runner.Config{
		SystemPrompt:             "",
		MaxTurns:                 0,
		AnthropicAuthToken:       wsTok,
		AnthropicBaseURL:         w.deps.config.LLMProxyURL,
		DisableFileCheckpointing: true,
		AutoCompactWindow:        165000,
		SessionID:                turn.SessionID,
		TurnID:                   turn.ID,
		ChannelType:              channelType,
		CreatorUserID:            meta.CreatorUserID,
		PermissionMode:           permMode,
		Model:                    meta.Model,
		PreferredExecutorID:      meta.PreferredExecutorID,
		TurnKind:                 turnKind,
	}

	msgCh, err := w.deps.runnerRun(turnCtx, ws, turn.SessionID, turn.UserMessage, runCfg, mcp)
	if err != nil {
		w.failTurn(ctx, turn, "runner.Run: "+err.Error())
		return
	}

	epoch, err := w.deps.store.GetSessionEpoch(ctx, turn.SessionID)
	if err != nil {
		w.deps.logger.Warn("get epoch failed", "session_id", turn.SessionID, "error", err)
	}

	for sdkMsg := range msgCh {
		evt, convErr := runner.ToEventPayload(sdkMsg)
		if convErr != nil {
			w.deps.logger.Warn("ToEventPayload failed",
				"session_id", turn.SessionID, "error", convErr)
			continue
		}
		eventID := uuid.NewString()
		var seqNum int64
		if !evt.Ephemeral {
			inserted, insertErr := w.deps.store.InsertEventsWithTurn(
				context.Background(), turn.SessionID, epoch, turn.ID,
				[]EventInput{{EventID: eventID, Payload: evt.Payload, Ephemeral: false}},
			)
			if insertErr != nil {
				w.deps.logger.Warn("InsertEventsWithTurn failed",
					"session_id", turn.SessionID, "error", insertErr)
			} else if len(inserted) > 0 {
				seqNum = inserted[0].SeqNum
			}
		}
		w.deps.sse.Publish(turn.SessionID, &StreamClientEvent{
			EventID:     eventID,
			SequenceNum: seqNum,
			EventType:   "client_event",
			Source:      "worker",
			TurnID:      turn.ID,
			Payload:     evt.Payload,
			CreatedAt:   time.Now().Format(time.RFC3339Nano),
		})
	}

	if turnCtx.Err() != nil {
		_ = w.deps.store.MarkTurnCancelled(context.Background(), turn.ID)
		w.publishTerminal(turn, "turn_cancelled")
		return
	}
	_ = w.deps.store.MarkTurnDone(context.Background(), turn.ID)
	w.publishTerminal(turn, "turn_done")
}

func (w *sessionWorker) failTurn(_ context.Context, turn *AgentTurn, msg string) {
	w.deps.logger.Error("turn failed",
		"session_id", turn.SessionID, "turn_id", turn.ID, "error", msg)
	_ = w.deps.store.MarkTurnFailed(context.Background(), turn.ID, msg)
	payload, _ := json.Marshal(map[string]string{"turn_id": turn.ID, "error": msg})
	w.deps.sse.Publish(turn.SessionID, &StreamClientEvent{
		EventID:   "evt_" + uuid.NewString(),
		EventType: "turn_failed",
		Source:    "worker",
		TurnID:    turn.ID,
		Payload:   payload,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	})
}

func (w *sessionWorker) publishTerminal(turn *AgentTurn, eventType string) {
	payload, _ := json.Marshal(map[string]string{"turn_id": turn.ID})
	w.deps.sse.Publish(turn.SessionID, &StreamClientEvent{
		EventID:   "evt_" + uuid.NewString(),
		EventType: eventType,
		Source:    "worker",
		TurnID:    turn.ID,
		Payload:   payload,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	})
}
```

Add imports to `session_worker.go`: `encoding/json`, `github.com/google/uuid`.

- [ ] **Step 4: Run the happy-path test**

```bash
go test ./internal/ccbroker/ -run TestExecuteHappyPath -v
```

Expected: PASS.

- [ ] **Step 5: Add cancel-mid-execute test**

Append to `session_worker_test.go`:

```go
func TestExecuteCancelMidStream(t *testing.T) {
	sid, wid, tid := "sess_c", "ws_c", "trn_c"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt", UserMessage: "x",
	})

	deps := workerDeps{
		store: store, sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		gate:   tools.NewGate(func(string, tools.Event) {}),
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient: http.DefaultClient,
		wstoken:    func(context.Context, string) (string, error) { return "t", nil },
		workspaceSetup: func(context.Context, string, string, *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{}, nil
		},
		workspaceTeardown: func(context.Context, *workspace.Workspace, *workspace.S3Store) error { return nil },
		runnerRun: func(ctx context.Context, _ *workspace.Workspace, _, _ string, _ runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			ch := make(chan agentsdk.SDKMessage)
			go func() {
				<-ctx.Done() // block until cancelled, then close
				close(ch)
			}()
			return ch, nil
		},
		callTurnFinished: func(string, string) {},
	}
	w := newSessionWorker(sid, deps, func(string) {})
	turn, _ := store.PickNextPending(context.Background(), sid)

	// Cancel via activeTurns from another goroutine after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		deps.activeTurns.Cancel(sid, tid)
	}()

	w.execute(context.Background(), turn)
	got, _ := store.GetTurn(context.Background(), tid)
	if got == nil || got.State != "cancelled" {
		t.Fatalf("expected cancelled, got %+v", got)
	}
}
```

Run: `go test ./internal/ccbroker/ -run TestExecuteCancelMidStream -v`
Expected: PASS.

- [ ] **Step 6: Add runner-error test**

```go
func TestExecuteRunnerError(t *testing.T) {
	sid, wid, tid := "sess_re", "ws_re", "trn_re"
	store := newFakeStore()
	store.sessions[sid] = &Session{ID: sid, WorkspaceID: wid}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: tid, SessionID: sid, WorkspaceID: wid, UserEventID: "evt", UserMessage: "x",
	})

	deps := workerDeps{
		store: store, sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		gate:   tools.NewGate(func(string, tools.Event) {}),
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		httpClient: http.DefaultClient,
		wstoken:    func(context.Context, string) (string, error) { return "t", nil },
		workspaceSetup: func(context.Context, string, string, *workspace.S3Store) (*workspace.Workspace, error) {
			return &workspace.Workspace{}, nil
		},
		workspaceTeardown: func(context.Context, *workspace.Workspace, *workspace.S3Store) error { return nil },
		runnerRun: func(context.Context, *workspace.Workspace, string, string, runner.Config, *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			return nil, errors.New("boom")
		},
		callTurnFinished: func(string, string) {},
	}
	w := newSessionWorker(sid, deps, func(string) {})
	turn, _ := store.PickNextPending(context.Background(), sid)
	w.execute(context.Background(), turn)
	got, _ := store.GetTurn(context.Background(), tid)
	if got == nil || got.State != "failed" {
		t.Fatalf("expected failed, got %+v", got)
	}
}
```

Add `errors` to imports.

Run: `go test ./internal/ccbroker/ -run TestExecute -v`
Expected: ALL PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/ccbroker/session_worker.go internal/ccbroker/session_worker_test.go internal/ccbroker/models.go
git commit -m "feat(ccbroker): implement sessionWorker.execute heavy path"
```

---

## Task 6: `workerRegistry`

**Files:**
- Create: `internal/ccbroker/worker_registry.go`
- Create: `internal/ccbroker/worker_registry_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/ccbroker/worker_registry_test.go`:

```go
package ccbroker

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryNotifySpawnsThenSignals(t *testing.T) {
	deps := workerDeps{
		store:  newFakeStore(),
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	r := newWorkerRegistry(deps)
	defer r.Shutdown(context.Background())

	r.Notify("sess_a")
	if r.workerCount() != 1 {
		t.Fatalf("expected 1 worker, got %d", r.workerCount())
	}
	r.Notify("sess_a")
	if r.workerCount() != 1 {
		t.Fatalf("Notify should be idempotent; got %d", r.workerCount())
	}
	r.Notify("sess_b")
	if r.workerCount() != 2 {
		t.Fatalf("expected 2 workers, got %d", r.workerCount())
	}
}

func TestRegistryShutdownClosesWorkers(t *testing.T) {
	store := newFakeStore()
	var executed atomic.Int32
	deps := workerDeps{
		store:  store,
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	r := newWorkerRegistry(deps)
	r.executeOverride = func(_ context.Context, _ *AgentTurn) { executed.Add(1) }
	r.Notify("sess_s")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if r.workerCount() != 0 {
		t.Fatalf("expected 0 workers after Shutdown, got %d", r.workerCount())
	}
}

func TestRegistryOnIdleExitUnregisters(t *testing.T) {
	deps := workerDeps{
		store:  newFakeStore(),
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	r := newWorkerRegistry(deps)
	r.idleTimeout = 50 * time.Millisecond
	r.Notify("sess_i")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.workerCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker did not idle-exit; count=%d", r.workerCount())
}
```

Run: `go test ./internal/ccbroker/ -run TestRegistry -v`
Expected: FAIL — `workerRegistry` undefined.

- [ ] **Step 2: Write `worker_registry.go`**

```go
package ccbroker

import (
	"context"
	"sync"
	"time"
)

// workerRegistry owns the per-session sessionWorker pool. Producers call
// Notify(sid); the registry spawns a worker if none exists, otherwise
// signals the existing one. Workers self-unregister via onIdleExit.
type workerRegistry struct {
	mu          sync.Mutex
	workers     map[string]*sessionWorker
	deps        workerDeps
	ctx         context.Context
	cancel      context.CancelFunc
	idleTimeout time.Duration

	// executeOverride lets tests stub the heavy path. Production leaves nil.
	executeOverride func(ctx context.Context, t *AgentTurn)
}

func newWorkerRegistry(deps workerDeps) *workerRegistry {
	ctx, cancel := context.WithCancel(context.Background())
	return &workerRegistry{
		workers:     make(map[string]*sessionWorker),
		deps:        deps,
		ctx:         ctx,
		cancel:      cancel,
		idleTimeout: defaultWorkerIdleTimeout,
	}
}

func (r *workerRegistry) Notify(sessionID string) {
	r.mu.Lock()
	w, ok := r.workers[sessionID]
	if !ok {
		w = newSessionWorker(sessionID, r.deps, r.onIdleExit)
		w.idleAfter = r.idleTimeout
		if r.executeOverride != nil {
			w.executeFn = r.executeOverride
		}
		r.workers[sessionID] = w
		go w.run(r.ctx)
	}
	r.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (r *workerRegistry) onIdleExit(sessionID string) {
	r.mu.Lock()
	delete(r.workers, sessionID)
	r.mu.Unlock()
}

func (r *workerRegistry) workerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.workers)
}

func (r *workerRegistry) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	workers := make([]*sessionWorker, 0, len(r.workers))
	for _, w := range r.workers {
		workers = append(workers, w)
	}
	r.mu.Unlock()
	for _, w := range workers {
		select {
		case <-w.quit:
		default:
			close(w.quit)
		}
	}
	r.cancel()

	deadline := time.Now().Add(2 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for time.Now().Before(deadline) {
		if r.workerCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	r.workers = map[string]*sessionWorker{}
	r.mu.Unlock()
	return nil
}
```

Run: `go test ./internal/ccbroker/ -run TestRegistry -v`
Expected: ALL PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/ccbroker/worker_registry.go internal/ccbroker/worker_registry_test.go
git commit -m "feat(ccbroker): add workerRegistry with idle-exit and shutdown"
```

---

## Task 7: Recovery on startup

**Files:**
- Create: `internal/ccbroker/recovery.go`
- Create: `internal/ccbroker/recovery_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/ccbroker/recovery_test.go`:

```go
package ccbroker

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

func TestRecoveryResetsRunningAndNotifies(t *testing.T) {
	store := newFakeStore()
	// Simulate a running turn left over from a crashed pod.
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_r1", SessionID: "sess_r1", WorkspaceID: "ws", UserEventID: "e", UserMessage: "x",
	})
	_ = store.MarkTurnRunning(context.Background(), "trn_r1")
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_r2", SessionID: "sess_r2", WorkspaceID: "ws", UserEventID: "e2", UserMessage: "y",
	})

	registry := newWorkerRegistry(workerDeps{
		store:  store,
		logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	defer registry.Shutdown(context.Background())

	s := &Server{
		store:          store,
		workerRegistry: registry,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	if err := s.recoverPendingTurns(context.Background()); err != nil {
		t.Fatalf("recoverPendingTurns: %v", err)
	}
	t1, _ := store.GetTurn(context.Background(), "trn_r1")
	if t1.State != "queued" {
		t.Fatalf("expected trn_r1 reset to queued, got %s", t1.State)
	}
	if registry.workerCount() != 2 {
		t.Fatalf("expected 2 workers notified, got %d", registry.workerCount())
	}
}
```

This will fail because `Server.workerRegistry` field and `recoverPendingTurns` method don't exist yet. We'll add them in Task 11; for now, add the bare minimum so the test compiles.

- [ ] **Step 2: Add `workerRegistry` field to `Server` (skeleton only)**

In `internal/ccbroker/server.go`, add to the `Server` struct (after `compactQueue *compactQueue`):

```go
	workerRegistry *workerRegistry
```

(Do not modify `NewServer` yet — Task 11 does that. Tests can construct Server literals directly.)

- [ ] **Step 3: Write `recovery.go`**

Create `internal/ccbroker/recovery.go`:

```go
package ccbroker

import (
	"context"
	"fmt"
)

// recoverPendingTurns is called once at Server startup before HTTP serving.
// It finds turns left in 'running' state by a crashed prior pod, resets
// them to 'queued', then notifies one worker per session that has any
// pending work so the queue drains immediately.
func (s *Server) recoverPendingTurns(ctx context.Context) error {
	n, err := s.store.ResetRunningToQueued(ctx)
	if err != nil {
		return fmt.Errorf("reset running to queued: %w", err)
	}
	if n > 0 {
		s.logger.Info("recovery: reset stale running turns", "count", n)
	}
	sids, err := s.store.ListSessionsWithPending(ctx)
	if err != nil {
		return fmt.Errorf("list sessions with pending: %w", err)
	}
	for _, sid := range sids {
		s.workerRegistry.Notify(sid)
	}
	s.logger.Info("recovery: notified workers", "session_count", len(sids))
	return nil
}
```

- [ ] **Step 4: Run the test**

```bash
go test ./internal/ccbroker/ -run TestRecoveryResetsRunningAndNotifies -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/recovery.go internal/ccbroker/recovery_test.go internal/ccbroker/server.go
git commit -m "feat(ccbroker): add recoverPendingTurns startup hook"
```

---

## Task 8: Refactor `handler_turns.go` to enqueue + stream-from-SSE

**Files:**
- Modify: `internal/ccbroker/handler_turns.go`
- Modify: `internal/ccbroker/handler_turns_test.go`

This is the cutover: the handler stops calling `runner.Run` directly. It now persists user message + turn row, calls `Notify`, and streams SSE filtered by `turn_id` until terminal.

- [ ] **Step 1: Write the test for the new contract**

Append to `handler_turns_test.go`:

```go
// TestHandleProcessTurn_EnqueuesAndStreams asserts that POST /api/turns
// inserts an agent_turns row, notifies the worker, then streams SSE events
// tagged with that turn_id until a terminal event arrives.
func TestHandleProcessTurn_EnqueuesAndStreams(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	registry := newWorkerRegistry(workerDeps{
		store: store, sse: sse,
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer registry.Shutdown(context.Background())

	// Stub the worker's execute to: mark running, publish 1 event tagged with
	// turn_id, mark done, publish terminal turn_done.
	registry.executeOverride = func(_ context.Context, turn *AgentTurn) {
		_ = store.MarkTurnRunning(context.Background(), turn.ID)
		payload, _ := json.Marshal(map[string]string{"text": "hi"})
		sse.Publish(turn.SessionID, &StreamClientEvent{
			EventID:   "evt_1",
			EventType: "client_event",
			TurnID:    turn.ID,
			Payload:   payload,
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		})
		_ = store.MarkTurnDone(context.Background(), turn.ID)
		sse.Publish(turn.SessionID, &StreamClientEvent{
			EventID:   "evt_term",
			EventType: "turn_done",
			TurnID:    turn.ID,
			Payload:   json.RawMessage(`{}`),
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		})
	}

	srv := &Server{
		store:          store,
		sse:            sse,
		activeTurns:    newActiveTurnRegistry(),
		compactQueue:   newCompactQueue(),
		workerRegistry: registry,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	body := bytes.NewBufferString(`{"session_id":"sess_h","workspace_id":"ws_h","user_message":"hello"}`)
	req := httptest.NewRequest("POST", "/api/turns", body)
	rec := httptest.NewRecorder()
	srv.handleProcessTurn(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"event_type":"client_event"`) {
		t.Fatalf("expected client_event in stream, got %s", out)
	}
	if !strings.Contains(out, `"event_type":"turn_done"`) {
		t.Fatalf("expected turn_done in stream, got %s", out)
	}
	if !strings.Contains(out, `"event_type":"done"`) {
		t.Fatalf("expected done sentinel, got %s", out)
	}
	// One agent_turns row should exist for the session.
	turns, _ := store.ListSessionTurns(context.Background(), "sess_h", 10)
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn row, got %d", len(turns))
	}
	if turns[0].State != "done" {
		t.Fatalf("expected state=done, got %s", turns[0].State)
	}
}
```

Add imports if missing: `bytes`, `io`.

Run: `go test ./internal/ccbroker/ -run TestHandleProcessTurn_EnqueuesAndStreams -v`
Expected: FAIL — current `handleProcessTurn` calls `runner.Run` directly, the stub never runs.

- [ ] **Step 2: Rewrite `handleProcessTurn`**

Replace the entire body of `internal/ccbroker/handler_turns.go` (excluding package + imports + `TurnMetadata` + `ProcessTurnRequest` + `defaultStr` which are kept) with:

```go
package ccbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// TurnMetadata carries optional per-turn metadata sent by TUI / API callers.
type TurnMetadata struct {
	ChannelType         string `json:"channel_type,omitempty"`
	CreatorUserID       string `json:"creator_user_id,omitempty"`
	PermissionMode      string `json:"permission_mode,omitempty"`
	Model               string `json:"model,omitempty"`
	PreferredExecutorID string `json:"preferred_executor_id,omitempty"`
	TurnKind            string `json:"turn_kind,omitempty"`
}

type ProcessTurnRequest struct {
	SessionID   string       `json:"session_id"`
	WorkspaceID string       `json:"workspace_id"`
	UserMessage string       `json:"user_message"`
	IMChannelID string       `json:"im_channel_id,omitempty"`
	IMUserID    string       `json:"im_user_id,omitempty"`
	Metadata    TurnMetadata `json:"metadata,omitempty"`
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

const maxPendingPerSession = 16

// handleProcessTurn is the synchronous wrapper around the async queue. It:
//   1. Validates and persists the user message + agent_turns row
//   2. Notifies the per-session worker
//   3. Subscribes to SSEBroker, filters by this turn_id, streams to client
//   4. Returns when a terminal event for this turn arrives, or on disconnect
//
// The handler never calls runner.Run; the worker does.
func (s *Server) handleProcessTurn(w http.ResponseWriter, r *http.Request) {
	var req ProcessTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SessionID == "" || req.WorkspaceID == "" || req.UserMessage == "" {
		writeError(w, http.StatusBadRequest, "session_id, workspace_id, and user_message are required")
		return
	}

	// Ensure session exists.
	sess, err := s.store.GetSession(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check session")
		return
	}
	if sess == nil {
		if err := s.store.CreateSession(r.Context(), req.SessionID, req.WorkspaceID, "", "api", nil); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
	}

	// Per-session backpressure.
	pending, err := s.store.CountPending(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check pending depth")
		return
	}
	if pending >= maxPendingPerSession {
		writeError(w, http.StatusTooManyRequests, "too many pending turns for this session")
		return
	}

	turnID := "trn_" + uuid.NewString()

	epoch, err := s.store.GetSessionEpoch(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get session epoch")
		return
	}

	userEventID := uuid.NewString()
	userPayload, _ := json.Marshal(map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": req.UserMessage,
		},
		"parent_tool_use_id": nil,
		"session_id":         req.SessionID,
	})
	if _, err := s.store.InsertEventsWithTurn(r.Context(), req.SessionID, epoch, turnID, []EventInput{
		{EventID: userEventID, Payload: userPayload, Ephemeral: false},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to insert user message")
		return
	}

	metaBytes, _ := json.Marshal(req.Metadata)
	turn := AgentTurn{
		ID:          turnID,
		SessionID:   req.SessionID,
		WorkspaceID: req.WorkspaceID,
		UserEventID: userEventID,
		UserMessage: req.UserMessage,
		Metadata:    metaBytes,
	}
	if req.IMChannelID != "" {
		turn.IMChannelID.String, turn.IMChannelID.Valid = req.IMChannelID, true
	}
	if req.IMUserID != "" {
		turn.IMUserID.String, turn.IMUserID.Valid = req.IMUserID, true
	}
	if err := s.store.EnqueueTurn(r.Context(), turn); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue turn")
		return
	}

	// Subscribe BEFORE notifying so we can't miss the worker's first event.
	sub := s.sse.Subscribe(req.SessionID)
	defer s.sse.Unsubscribe(req.SessionID, sub)

	s.workerRegistry.Notify(req.SessionID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Send turn_id in the prelude so clients know which turn this stream
	// represents.
	fmt.Fprintf(w, "data: {\"event_type\":\"turn_started\",\"turn_id\":%q}\n\n", turnID)
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected; the worker keeps running. Cancel is a
			// separate explicit endpoint.
			return
		case evt := <-sub.Ch:
			if evt.TurnID != "" && evt.TurnID != turnID {
				continue // belongs to another turn on this session
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if isTerminalEventType(evt.EventType) && evt.TurnID == turnID {
				fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", turnID)
				flusher.Flush()
				return
			}
		case <-sub.Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		}
	}
}

func isTerminalEventType(t string) bool {
	switch t {
	case "turn_done", "turn_cancelled", "turn_failed":
		return true
	}
	return false
}

// callTurnFinished is preserved on Server (turn_finished.go).
// Keep this file producing that hook reference indirect to avoid a circular edit.
var _ = context.Background
```

(The `var _ = context.Background` is a deliberate guard so removing context import inadvertently surfaces in CI; remove if linter complains.)

Note we drop the previous package-level seam vars `workspaceSetup`, `workspaceTeardown`, `runnerRun` from this file because they moved into `workerDeps`. Keep them in `session_worker.go` if any other consumer relies on them — search confirms only this file used them at package level.

- [ ] **Step 3: Verify package-level seams are removed and search for leftovers**

```bash
grep -n "workspaceSetup\|workspaceTeardown\|runnerRun" internal/ccbroker/
```

Should appear ONLY inside `session_worker.go`, `session_worker_test.go`, and `handler_turns_test.go` (the latter for the previous package seam, which is now obsolete). Update `handler_turns_test.go`'s previous `workspaceSetup = ...` patch sections in the older `TestHandleProcessTurn_OrchestratesPipeline` test (which is `t.Skip`ped today) — replace those with a comment explaining the seams moved to `workerDeps`, or delete the skipped test entirely since the new `TestHandleProcessTurn_EnqueuesAndStreams` supersedes it.

- [ ] **Step 4: Delete the skipped legacy test**

In `handler_turns_test.go`, delete `TestHandleProcessTurn_OrchestratesPipeline` and any helper that referenced the old package seams (`origSetup`, etc.). Also delete `buildTestServer` which is `t.Skip`ped — the new test constructs the Server literal directly.

- [ ] **Step 5: Run the new test**

```bash
go test ./internal/ccbroker/ -run TestHandleProcessTurn_EnqueuesAndStreams -v
```

Expected: PASS.

- [ ] **Step 6: Run all ccbroker tests**

```bash
go test ./internal/ccbroker/ -v
```

Expected: PASS for all non-skipped tests.

- [ ] **Step 7: Add depth-limit test**

Append to `handler_turns_test.go`:

```go
func TestHandleProcessTurn_DepthLimit(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	registry := newWorkerRegistry(workerDeps{
		store: store, sse: sse,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	registry.executeOverride = func(context.Context, *AgentTurn) {} // no-op; turns stay queued
	defer registry.Shutdown(context.Background())

	srv := &Server{
		store: store, sse: sse,
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		workerRegistry: registry,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Pre-fill 16 queued turns directly via store.
	for i := 0; i < maxPendingPerSession; i++ {
		_ = store.EnqueueTurn(context.Background(), AgentTurn{
			ID: fmt.Sprintf("trn_%d", i), SessionID: "sess_d", WorkspaceID: "ws", UserEventID: "e", UserMessage: "x",
		})
	}
	store.sessions["sess_d"] = &Session{ID: "sess_d", WorkspaceID: "ws"}

	body := bytes.NewBufferString(`{"session_id":"sess_d","workspace_id":"ws","user_message":"overflow"}`)
	req := httptest.NewRequest("POST", "/api/turns", body)
	rec := httptest.NewRecorder()
	srv.handleProcessTurn(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}
```

Run: `go test ./internal/ccbroker/ -run TestHandleProcessTurn_DepthLimit -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/ccbroker/handler_turns.go internal/ccbroker/handler_turns_test.go
git commit -m "refactor(ccbroker): handler enqueues turn and streams from SSE; drop turnLock path"
```

---

## Task 9: State-aware cancel for queued turns

**Files:**
- Modify: `internal/ccbroker/handler_tui_routes.go`
- Modify: `internal/ccbroker/handler_tui_routes_test.go`

- [ ] **Step 1: Write the failing test**

Append to `handler_tui_routes_test.go` a test that posts a queued turn, calls cancel, and asserts state transitions to `cancelled`:

```go
func TestCancelQueuedTurn(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	srv := &Server{
		store: store, sse: sse,
		activeTurns: newActiveTurnRegistry(),
		gate:        tools.NewGate(func(string, tools.Event) {}),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	store.sessions["sess_q"] = &Session{ID: "sess_q", WorkspaceID: "ws"}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_q", SessionID: "sess_q", WorkspaceID: "ws", UserEventID: "e", UserMessage: "x",
	})

	r := chi.NewRouter()
	r.Post("/api/sessions/{sid}/turns/{tid}/cancel", srv.handleCancelTurn)
	req := httptest.NewRequest("POST", "/api/sessions/sess_q/turns/trn_q/cancel", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("expected 200/202, got %d", rec.Code)
	}
	got, _ := store.GetTurn(context.Background(), "trn_q")
	if got.State != "cancelled" {
		t.Fatalf("expected cancelled, got %s", got.State)
	}
}

func TestCancelTerminalTurnReturns410(t *testing.T) {
	store := newFakeStore()
	srv := &Server{
		store: store, sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(),
		gate:        tools.NewGate(func(string, tools.Event) {}),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_t", SessionID: "sess_t", WorkspaceID: "ws", UserEventID: "e", UserMessage: "x",
	})
	_ = store.MarkTurnDone(context.Background(), "trn_t")

	r := chi.NewRouter()
	r.Post("/api/sessions/{sid}/turns/{tid}/cancel", srv.handleCancelTurn)
	req := httptest.NewRequest("POST", "/api/sessions/sess_t/turns/trn_t/cancel", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", rec.Code)
	}
}
```

Add imports: `bytes`, `context`, `io`, `net/http`, `net/http/httptest`, `log/slog`, `github.com/go-chi/chi/v5`, `github.com/agentserver/agentserver/internal/ccbroker/tools`.

Run: `go test ./internal/ccbroker/ -run TestCancel -v`
Expected: FAIL — current handler doesn't consult store, no state distinction.

- [ ] **Step 2: Rewrite `handleCancelTurn`**

In `internal/ccbroker/handler_tui_routes.go`, replace `handleCancelTurn`:

```go
func (s *Server) handleCancelTurn(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	tid := chi.URLParam(r, "tid")

	turn, err := s.store.GetTurn(r.Context(), tid)
	if err != nil {
		http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
		return
	}
	if turn == nil || turn.SessionID != sid {
		http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
		return
	}

	switch turn.State {
	case "queued":
		if err := s.store.MarkTurnCancelled(r.Context(), tid); err != nil {
			http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
			return
		}
		s.gate.CancelTurn(tid)
		s.broadcastTurnCancelled(sid, tid)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"cancelled":true,"was":"queued"}`))
	case "running":
		s.activeTurns.Cancel(sid, tid)
		s.gate.CancelTurn(tid)
		s.broadcastTurnCancelled(sid, tid)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"cancelled":true,"was":"running"}`))
	default:
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(`{"code":"already_terminal","state":"` + turn.State + `"}`))
	}
}

func (s *Server) broadcastTurnCancelled(sid, tid string) {
	payload, _ := json.Marshal(map[string]string{"turn_id": tid})
	s.sse.Publish(sid, &StreamClientEvent{
		EventID:   "evt_" + uuid.NewString(),
		EventType: "turn_cancelled",
		Source:    "broker",
		TurnID:    tid,
		Payload:   payload,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	})
}
```

- [ ] **Step 3: Run cancel tests**

```bash
go test ./internal/ccbroker/ -run TestCancel -v
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/ccbroker/handler_tui_routes.go internal/ccbroker/handler_tui_routes_test.go
git commit -m "feat(ccbroker): cancel handles queued + running + terminal states"
```

---

## Task 10: New endpoint `GET /api/turns/{tid}/events` (catch-up + tail)

**Files:**
- Create: `internal/ccbroker/handler_turn_events.go`
- Create: `internal/ccbroker/handler_turn_events_test.go`
- Modify: `internal/ccbroker/store.go` (add `GetTurnEvents`)
- Modify: `internal/ccbroker/agent_turns.go` (extend storer + Store with `GetTurnEvents`)

The endpoint streams events for a single turn:
- query `?since=N` to skip events with seq_num ≤ N
- catch-up first from `agent_session_events` where `turn_id=$1`
- then live-tail SSEBroker filtered by turn_id
- close on terminal event for this turn

- [ ] **Step 1: Add `GetTurnEvents` to storer + Store + fakeStore**

In `store.go`, add to the `storer` interface:

```go
	GetTurnEvents(ctx context.Context, turnID string, sinceSeqNum int64) ([]TurnEvent, error)
```

In `agent_turns.go`, add the type and method:

```go
type TurnEvent struct {
	SeqNum    int64
	EventID   string
	EventType string
	Payload   json.RawMessage
	CreatedAt time.Time
}

func (s *Store) GetTurnEvents(ctx context.Context, turnID string, sinceSeqNum int64) ([]TurnEvent, error) {
	rows, err := s.QueryContext(ctx,
		`SELECT id, event_id, event_type, payload, created_at
		 FROM agent_session_events
		 WHERE turn_id=$1 AND id > $2
		 ORDER BY id ASC`, turnID, sinceSeqNum)
	if err != nil {
		return nil, fmt.Errorf("get turn events: %w", err)
	}
	defer rows.Close()
	var out []TurnEvent
	for rows.Next() {
		var e TurnEvent
		if err := rows.Scan(&e.SeqNum, &e.EventID, &e.EventType, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan turn event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

In `handler_turns_test.go`, extend `fakeStore`:

```go
func (f *fakeStore) GetTurnEvents(_ context.Context, turnID string, sinceSeqNum int64) ([]TurnEvent, error) {
	// fakeStore doesn't track turn_id on events; return empty for tests that
	// don't exercise catch-up. handler_turn_events_test.go installs its own
	// fixture if needed.
	return nil, nil
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/ccbroker/handler_turn_events_test.go`:

```go
package ccbroker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestTurnEventsStreamsLiveAndTerminates(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	store.sessions["sess_e"] = &Session{ID: "sess_e", WorkspaceID: "ws"}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_e", SessionID: "sess_e", WorkspaceID: "ws", UserEventID: "u", UserMessage: "x",
	})
	srv := &Server{
		store: store, sse: sse,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	r := chi.NewRouter()
	r.Get("/api/turns/{tid}/events", srv.handleTurnEvents)

	req := httptest.NewRequest("GET", "/api/turns/trn_e/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { r.ServeHTTP(rec, req); close(done) }()

	// Give the handler a moment to subscribe.
	time.Sleep(50 * time.Millisecond)
	payload, _ := json.Marshal(map[string]string{"x": "y"})
	sse.Publish("sess_e", &StreamClientEvent{
		EventID: "evt_a", EventType: "client_event", TurnID: "trn_e", Payload: payload,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	})
	sse.Publish("sess_e", &StreamClientEvent{
		EventID: "evt_b", EventType: "turn_done", TurnID: "trn_e", Payload: json.RawMessage(`{}`),
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not return after terminal event")
	}
	out := rec.Body.String()
	if !strings.Contains(out, "client_event") || !strings.Contains(out, "turn_done") {
		t.Fatalf("missing events in stream: %s", out)
	}
}

func TestTurnEvents404OnUnknownTurn(t *testing.T) {
	store := newFakeStore()
	srv := &Server{store: store, sse: NewSSEBroker(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := chi.NewRouter()
	r.Get("/api/turns/{tid}/events", srv.handleTurnEvents)
	req := httptest.NewRequest("GET", "/api/turns/trn_unknown/events", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
```

Run: `go test ./internal/ccbroker/ -run TestTurnEvents -v`
Expected: FAIL — `handleTurnEvents` undefined.

- [ ] **Step 3: Implement `handleTurnEvents`**

Create `internal/ccbroker/handler_turn_events.go`:

```go
package ccbroker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleTurnEvents(w http.ResponseWriter, r *http.Request) {
	tid := chi.URLParam(r, "tid")

	turn, err := s.store.GetTurn(r.Context(), tid)
	if err != nil {
		http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
		return
	}
	if turn == nil {
		http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
		return
	}

	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe FIRST so we don't miss events that fire between catch-up and tail.
	sub := s.sse.Subscribe(turn.SessionID)
	defer s.sse.Unsubscribe(turn.SessionID, sub)

	// Catch-up from DB.
	seenSeqs := map[int64]struct{}{}
	highestSeq := since
	if past, err := s.store.GetTurnEvents(r.Context(), tid, since); err == nil {
		for _, e := range past {
			evt := &StreamClientEvent{
				EventID: e.EventID, SequenceNum: e.SeqNum, EventType: e.EventType,
				Source: "catchup", TurnID: tid, Payload: e.Payload,
				CreatedAt: e.CreatedAt.Format(time.RFC3339Nano),
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			seenSeqs[e.SeqNum] = struct{}{}
			if e.SeqNum > highestSeq {
				highestSeq = e.SeqNum
			}
			if isTerminalEventType(e.EventType) {
				fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", tid)
				flusher.Flush()
				return
			}
		}
		flusher.Flush()
	} else {
		s.logger.Warn("turn events catch-up failed", "turn_id", tid, "error", err)
	}

	// If turn was already terminal at request time and DB had no events past
	// since, end here. Otherwise tail.
	if isTerminalTurnState(turn.State) && len(seenSeqs) == 0 {
		fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", tid)
		flusher.Flush()
		return
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.Done():
			return
		case evt := <-sub.Ch:
			if evt.TurnID != tid {
				continue
			}
			if evt.SequenceNum != 0 {
				if _, dup := seenSeqs[evt.SequenceNum]; dup {
					continue
				}
				seenSeqs[evt.SequenceNum] = struct{}{}
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if isTerminalEventType(evt.EventType) {
				fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", tid)
				flusher.Flush()
				return
			}
		case <-keepalive.C:
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		}
	}
}

func isTerminalTurnState(state string) bool {
	switch state {
	case "done", "cancelled", "failed":
		return true
	}
	return false
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/ccbroker/ -run TestTurnEvents -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/handler_turn_events.go internal/ccbroker/handler_turn_events_test.go internal/ccbroker/store.go internal/ccbroker/agent_turns.go internal/ccbroker/handler_turns_test.go
git commit -m "feat(ccbroker): add GET /api/turns/{tid}/events catch-up + tail"
```

---

## Task 11: New endpoint `GET /api/sessions/{sid}/turns`

**Files:**
- Create: `internal/ccbroker/handler_session_turns.go`
- Create: `internal/ccbroker/handler_session_turns_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/ccbroker/handler_session_turns_test.go`:

```go
package ccbroker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestListSessionTurns(t *testing.T) {
	store := newFakeStore()
	store.sessions["sess_l"] = &Session{ID: "sess_l", WorkspaceID: "ws"}
	for i := 0; i < 3; i++ {
		_ = store.EnqueueTurn(context.Background(), AgentTurn{
			ID: "trn_l_" + string(rune('a'+i)), SessionID: "sess_l",
			WorkspaceID: "ws", UserEventID: "u", UserMessage: "x",
		})
	}
	srv := &Server{store: store,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := chi.NewRouter()
	r.Get("/api/sessions/{sid}/turns", srv.handleListSessionTurns)
	req := httptest.NewRequest("GET", "/api/sessions/sess_l/turns", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Turns []map[string]interface{} `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(resp.Turns))
	}
}
```

Run: `go test ./internal/ccbroker/ -run TestListSessionTurns -v`
Expected: FAIL — handler undefined.

- [ ] **Step 2: Implement the handler**

Create `internal/ccbroker/handler_session_turns.go`:

```go
package ccbroker

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

type sessionTurnRow struct {
	TurnID     string  `json:"turn_id"`
	State      string  `json:"state"`
	EnqueuedAt string  `json:"enqueued_at"`
	StartedAt  *string `json:"started_at,omitempty"`
	FinishedAt *string `json:"finished_at,omitempty"`
	ErrorMsg   *string `json:"error_msg,omitempty"`
}

func (s *Server) handleListSessionTurns(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	turns, err := s.store.ListSessionTurns(r.Context(), sid, limit)
	if err != nil {
		http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
		return
	}
	out := make([]sessionTurnRow, 0, len(turns))
	for _, t := range turns {
		row := sessionTurnRow{
			TurnID: t.ID, State: t.State,
			EnqueuedAt: t.EnqueuedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		}
		if t.StartedAt.Valid {
			s := t.StartedAt.Time.UTC().Format("2006-01-02T15:04:05.000000000Z")
			row.StartedAt = &s
		}
		if t.FinishedAt.Valid {
			s := t.FinishedAt.Time.UTC().Format("2006-01-02T15:04:05.000000000Z")
			row.FinishedAt = &s
		}
		if t.ErrorMsg.Valid {
			s := t.ErrorMsg.String
			row.ErrorMsg = &s
		}
		out = append(out, row)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"turns": out})
}
```

- [ ] **Step 3: Run the test**

```bash
go test ./internal/ccbroker/ -run TestListSessionTurns -v
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/ccbroker/handler_session_turns.go internal/ccbroker/handler_session_turns_test.go
git commit -m "feat(ccbroker): add GET /api/sessions/{sid}/turns"
```

---

## Task 12: Wire into Server, drop `turnLock`, register routes, add Start/Shutdown

**Files:**
- Modify: `internal/ccbroker/server.go`
- Delete: `internal/ccbroker/turn_lock.go`
- Modify: any callers of `NewServer` to call `Start` (e.g. `cmd/ccbroker/main.go` if present)

- [ ] **Step 1: Locate cc-broker main entrypoint**

```bash
grep -rln "ccbroker.NewServer" --include="*.go"
```

Note the file path (likely `cmd/ccbroker/main.go`). Open it.

- [ ] **Step 2: Drop `turnLock` from Server, add `workerRegistry` wiring**

In `internal/ccbroker/server.go`:

1. Remove the `turnLock *TurnLock` field. The `workerRegistry *workerRegistry` field was already added in Task 7.
2. In `NewServer`, after the existing `s.gate = ...` block, add:

```go
	deps := workerDeps{
		store:             store,
		s3:                s3,
		wstoken:           s.wstoken,
		sse:               s.sse,
		activeTurns:       s.activeTurns,
		compactQueue:      s.compactQueue,
		gate:              s.gate,
		logger:            logger,
		config:            cfg,
		httpClient:        http.DefaultClient,
		workspaceSetup:    workspace.Setup,
		workspaceTeardown: workspace.Teardown,
		runnerRun: func(ctx context.Context, ws *workspace.Workspace, sid, msg string, cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
			return runner.Run(ctx, ws, sid, msg, cfg, mcp)
		},
		callTurnFinished: s.callTurnFinished,
	}
	s.workerRegistry = newWorkerRegistry(deps)
```

3. Remove `turnLock: NewTurnLock(),` from the Server literal.

4. Add new methods:

```go
// Start runs one-time startup work (recovery) before HTTP serving begins.
func (s *Server) Start(ctx context.Context) error {
	return s.recoverPendingTurns(ctx)
}

// Shutdown stops worker goroutines and waits up to ctx deadline for them
// to drain. Best-effort: a stuck worker is abandoned and its turn will be
// reset on next process start.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.workerRegistry.Shutdown(ctx)
}
```

5. Add new routes in `Routes()`:

```go
	r.Get("/api/turns/{tid}/events",         s.handleTurnEvents)
	r.Get("/api/sessions/{sid}/turns",       s.handleListSessionTurns)
```

Add imports as needed: `context`, `github.com/agentserver/agentserver/internal/ccbroker/runner`, `agentsdk "github.com/agentserver/claude-agent-sdk-go"`.

- [ ] **Step 3: Delete `turn_lock.go`**

```bash
rm internal/ccbroker/turn_lock.go
```

- [ ] **Step 4: Update main.go to call Start before ListenAndServe**

In `cmd/ccbroker/main.go` (path discovered in Step 1), find the block that does `srv, err := ccbroker.NewServer(...)` and add immediately after:

```go
	if err := srv.Start(context.Background()); err != nil {
		log.Fatalf("ccbroker: recovery failed: %v", err)
	}
```

If the main loop has graceful shutdown via signal, ensure `srv.Shutdown(ctx)` is called. If not, leave that for a follow-up — Shutdown is best-effort and a process kill leaves work to be recovered on restart.

- [ ] **Step 5: Build the whole module**

```bash
go build ./...
```

Expected: PASS.

- [ ] **Step 6: Run the whole ccbroker test suite**

```bash
go test ./internal/ccbroker/... -v
```

Expected: ALL PASS.

- [ ] **Step 7: Run `go vet` and `gofmt -l`**

```bash
go vet ./internal/ccbroker/...
gofmt -l internal/ccbroker/
```

Expected: no output from `gofmt -l` (or fix what it reports).

- [ ] **Step 8: Commit**

```bash
git add -A internal/ccbroker/ cmd/ccbroker/
git commit -m "feat(ccbroker): wire workerRegistry, drop turnLock, add Start/Shutdown"
```

---

## Task 13: End-to-end smoke test (in-process)

**Files:**
- Create: `internal/ccbroker/e2e_queue_test.go`

Goal: prove the producer→worker→SSE handshake works as a unit, with the worker stubbed to emit one event + terminal.

- [ ] **Step 1: Write the test**

Create `internal/ccbroker/e2e_queue_test.go`:

```go
package ccbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/ccbroker/tools"
)

func TestE2E_QueueRoundTrip(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	deps := workerDeps{
		store: store, sse: sse,
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		gate:   tools.NewGate(func(string, tools.Event) {}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	registry := newWorkerRegistry(deps)
	registry.executeOverride = func(_ context.Context, t *AgentTurn) {
		_ = store.MarkTurnRunning(context.Background(), t.ID)
		sse.Publish(t.SessionID, &StreamClientEvent{
			EventID: "evt_x", EventType: "client_event", TurnID: t.ID,
			Payload: json.RawMessage(`{"text":"hi"}`),
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		})
		_ = store.MarkTurnDone(context.Background(), t.ID)
		sse.Publish(t.SessionID, &StreamClientEvent{
			EventID: "evt_term", EventType: "turn_done", TurnID: t.ID,
			Payload: json.RawMessage(`{}`),
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		})
	}
	defer registry.Shutdown(context.Background())

	srv := &Server{
		store:          store,
		sse:            sse,
		activeTurns:    deps.activeTurns,
		compactQueue:   deps.compactQueue,
		gate:           deps.gate,
		workerRegistry: registry,
		logger:         deps.logger,
	}

	body := bytes.NewBufferString(`{"session_id":"sess_e2e","workspace_id":"ws","user_message":"ping"}`)
	req := httptest.NewRequest("POST", "/api/turns", body)
	rec := httptest.NewRecorder()
	srv.handleProcessTurn(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{"turn_started", "client_event", "turn_done", `"event_type":"done"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in stream: %s", want, out)
		}
	}
	turns, _ := store.ListSessionTurns(context.Background(), "sess_e2e", 10)
	if len(turns) != 1 || turns[0].State != "done" {
		t.Fatalf("expected 1 done turn, got %+v", turns)
	}
}
```

- [ ] **Step 2: Run it**

```bash
go test ./internal/ccbroker/ -run TestE2E_QueueRoundTrip -v
```

Expected: PASS.

- [ ] **Step 3: Final full-suite run**

```bash
go test ./... 2>&1 | tail -50
```

Expected: PASS (excluding any pre-existing skipped tests outside this scope).

- [ ] **Step 4: Commit**

```bash
git add internal/ccbroker/e2e_queue_test.go
git commit -m "test(ccbroker): e2e smoke test for queue round-trip"
```

---

## Self-review checklist (post-plan)

**Spec coverage:**
- ✅ Migration 002 → Task 1
- ✅ `agent_turns.go` store ops → Task 2
- ✅ `session_worker.go` (run + execute) → Tasks 4, 5
- ✅ `worker_registry.go` → Task 6
- ✅ `recovery.go` → Task 7
- ✅ `handler_turns.go` sync wrapper → Task 8 (with depth check)
- ✅ `handler_tui_routes.go` cancel queued + running → Task 9
- ✅ `handler_turn_events.go` catch-up + tail → Task 10
- ✅ `handler_session_turns.go` list turns → Task 11
- ✅ `server.go` Start/Shutdown/Routes + drop turnLock → Task 12
- ✅ `models.go` `TurnID` field → Task 5
- ✅ `InsertEventsWithTurn` → Task 2
- ✅ E2E smoke → Task 13
- ⛔ `POST /api/v2/turns` — explicitly **out of scope** for PR 1 per spec §"Migration plan — two PRs"
- ⛔ `internal/db/migrations/022_*.sql` path — replaced with `internal/ccbroker/migrations/002_*.sql` to match existing layout (documented in plan header)

**Type/method consistency check:**
- `storer` interface gains 12 methods; all 12 are implemented on `*Store` (Task 2) and on `*fakeStore` (Task 3, plus `GetTurnEvents` in Task 10).
- `AgentTurn` field names are stable across tasks: `IMChannelID`/`IMUserID` are `sql.NullString` everywhere (no `string` aliases later).
- `StreamClientEvent.TurnID` added in Task 5; consumed in Tasks 8, 9, 10, 13.
- `workerDeps.executeOverride` is on `workerRegistry`, not `workerDeps` — registry sets `w.executeFn` from override at spawn time.
- `defaultStr` and `TurnMetadata` are kept in `handler_turns.go` (Task 8 preserves them); `session_worker.go` (Task 5) imports them from the same package.
- `isTerminalEventType` is defined in `handler_turns.go` (Task 8); reused by `handler_turn_events.go` (Task 10) — same package, OK.

**Placeholder scan:** No "TODO" / "fill in later" / "similar to Task N (without code)" anywhere — every task includes the exact code or commands.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-04-ccbroker-async-turn-queue.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, two-stage review (spec compliance, then code quality) between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session with batch execution + checkpoints for review.

Which approach?
