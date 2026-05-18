# Operation Log Implementation Plan (Plan 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every `mcpServer/tool/call` going through `codex-app-gateway` (SDK or TUI sourced) gets append-only logged to a new `operations` PostgreSQL table; `agentserver_sdk`'s `await ctx.history(...)` returns real records via gateway-side `operations/list` interception.

**Architecture:** Three pieces. (1) `operations` table + agentserver `/internal/operations` POST/GET handlers (write + filtered list) + hourly retention goroutine. (2) `codex-app-gateway` `oplog` package: HTTP client that fire-and-forget POSTs to agentserver, and JSON-RPC frame Interceptor that decodes `mcpServer/tool/call` request/response pairs and submits. (3) New `/notebook/ws` route on the gateway for SDK traffic (source-tagged `sdk`); existing `/codex-app/ws` keeps TUI traffic (source-tagged `tui`). The Interceptor also intercepts `operations/list` SDK requests and serves them by calling agentserver — without forwarding to `codex app-server` (which has no such method).

**Tech Stack:** Go 1.26 · `database/sql` + `lib/pq` (matches existing agentserver pattern) · chi/v5 router · `nhooyr.io/websocket` · embedded migrations via `embed.FS` · agentserver `AgentserverInternalSecret` shared bearer for gateway→agentserver auth.

**Out of scope for this plan (separate work):**
- Web UI `<OperationsPanel />` (Plan 3)
- LLM-initiated tool call logging (v1.5 — env-mcp itself emits log records)
- Cell/notebook attribution (`notebook_path`, `cell_id` columns) — schema has the columns, kept NULL for v1
- Per-workspace retention overrides — uses the global default; pluggable later

---

## File Structure

```
internal/db/migrations/023_operations.sql                                       # NEW
internal/db/operations.go                                                       # NEW (Insert/List/PruneOlderThan/typed row)
internal/db/operations_test.go                                                  # NEW

internal/server/operations.go                                                   # NEW (POST + GET /internal/operations handlers; bearer-auth)
internal/server/operations_test.go                                              # NEW
internal/server/operations_retention.go                                         # NEW (StartRetention(ctx, db, ttl, every))
internal/server/operations_retention_test.go                                    # NEW

internal/codexappgateway/oplog/                                                 # NEW package
├── doc.go                                                                      # package summary
├── client.go                                                                   # HTTP client; async Submit(); bounded channel; drop+metric on overflow
├── client_test.go                                                              # httptest server
├── interceptor.go                                                              # JSON-RPC frame parser; per-conn pending map; source/user tag; truncation
├── interceptor_test.go
├── operations_list.go                                                          # intercept SDK 'operations/list' → call client.List → respond on ws
└── operations_list_test.go

internal/wsbridge/wsbridge.go                                                   # MODIFIED — add RunProxyWithInterceptor + Interceptor type
internal/wsbridge/wsbridge_test.go                                              # MODIFIED — cover new variant

internal/codexappgateway/server.go                                              # MODIFIED — /notebook/ws route + wire oplog
internal/codexappgateway/server_test.go                                         # MODIFIED — assert source tagging via test stub
internal/codexappgateway/config.go                                              # MODIFIED — new oplog config fields
internal/codexappgateway/buildconfig_test.go                                    # MODIFIED — covers new fields

deploy/helm/agentserver/values.yaml                                             # MODIFIED — oplog enable flag + retention days
deploy/helm/agentserver/templates/codex-app-gateway.yaml                        # MODIFIED — env vars for oplog endpoint + secret reference
```

---

## Design Decisions Locked Before Tasks

These come up across tasks. Capturing once.

**1. Gateway writes via HTTP, not direct PG.** Gateway is a separate pod with no DB credentials. agentserver owns the DB. Gateway POSTs `/internal/operations` with `Authorization: Bearer <AgentserverInternalSecret>` — same auth pattern already used for `/internal/auth/verify` etc. Keeps the gateway stateless and avoids leaking PG creds into a second service.

**2. `operations/list` intercepted in gateway (does NOT reach `codex app-server`).** codex has no such method; if forwarded it would error. Interceptor recognises the method on the request-direction frame, calls agentserver `/internal/operations` (GET with workspace_id + filters), writes the response back on the ws using the same JSON-RPC id. The client-direction frame for that request id is suppressed so it never tries to reach codex.

**3. Async oplog writes — fire-and-forget bounded channel (capacity 1024).** Submit returns immediately. A single background goroutine drains the channel and POSTs to agentserver. If channel is full → drop + bump a counter metric. Tool calls never block on logging. agentserver downtime is observable via the drop metric.

**4. Path-based source tagging.** New ws route `/notebook/ws` for SDK traffic; source = `"sdk"`. Existing `/codex-app/ws` route used by TUI; source = `"tui"`. Source is captured at proxy construction, not from any client-supplied tag. Avoids spoofing.

**5. user_id / workspace_id sourced from auth context.** workspace_id comes from `Identity.WorkspaceID` (already set by the bearer auth verifier, see `server.go:249`). user_id comes from the request's `_meta.agentserver_user_id` field if present (gateway trusts it because the workspace_id boundary is enforced upstream by the bearer); else NULL.

**6. Payload truncation:**
- `arguments` > 64 KiB JSON → store `NULL` and `arguments_meta = {"truncated":true,"size_bytes":N,"sha256":"…"}`
- `result.content[].text` joined → first 4 KiB → `result_summary`; remainder dropped, `result_meta = {"truncated":true,"total_bytes":N}`
- Non-text content blocks (image/resource_link) → not in `result_summary`; only sha256 + size recorded
- Apply BEFORE submitting to the bounded channel (truncate in the request goroutine, channel carries small structs)

**7. Retention.** Hourly goroutine in agentserver web. `DELETE FROM operations WHERE started_at < NOW() - INTERVAL '90 days'`. Default 90d, configurable via `helm values.operations.retentionDays`. Goroutine started in `main.go`'s server boot; cancelled on shutdown.

**8. Schema includes v1.5 columns from day one** (`notebook_path`, `cell_id`, `thread_id`). All nullable. v1 leaves them NULL — saves a future migration.

**9. JSON-RPC frame parsing is best-effort and non-fatal.** Interceptor never breaks frame forwarding. If a frame can't be parsed (binary, malformed JSON, unknown shape), it passes through untouched and is not logged. The `pump` keeps running.

**10. operations_list pagination:** SDK passes `limit` (default 100, max 1000). Server returns `{"operations": [...]}`. No cursor pagination in v1 (added when v1.5 cell_id filter lands).

**11. Go 1.26 + database/sql + lib/pq.** Matches the rest of agentserver. No new dep.

**12. Test isolation.** DB layer tests use a real PG via testcontainers OR an existing repo helper. Check `internal/db/` for the pattern before writing tests.

---

## Task 1: DB migration + queries

**Files:**
- Create: `internal/db/migrations/023_operations.sql`
- Create: `internal/db/operations.go`
- Create: `internal/db/operations_test.go`

- [ ] **Step 1: Inspect existing DB test pattern**

Read `internal/db/*_test.go` (pick one with table tests) to learn how tests get a `*DB`. Likely one of:
- `testutil`/`testdb` helper spinning up a PG via testcontainers or temp DB
- Skip-if-no-DSN env-var pattern

If the repo uses an env-var skip pattern, follow that for `operations_test.go`. If it uses a helper, import the helper. Document the choice in a comment at top of the test file.

- [ ] **Step 2: Write the migration**

Create `internal/db/migrations/023_operations.sql`:

```sql
-- v1 operation log. Written by codex-app-gateway via POST /internal/operations
-- whenever an mcpServer/tool/call request/response pair completes.
--
-- Columns marked v1.5 are populated only when env-mcp itself emits the record
-- (LLM-initiated calls) or when the IPython kernel attribution metadata is
-- wired up. v1 leaves them NULL.

CREATE TABLE operations (
  id              UUID PRIMARY KEY,
  workspace_id    TEXT NOT NULL,
  user_id         TEXT,
  source          TEXT NOT NULL,
  thread_id       TEXT,
  request_id      TEXT,

  env_id          TEXT NOT NULL,
  tool            TEXT NOT NULL,
  arguments       JSONB,
  arguments_meta  JSONB,

  is_error        BOOLEAN NOT NULL,
  result_summary  TEXT,
  result_meta     JSONB,

  started_at      TIMESTAMPTZ NOT NULL,
  completed_at    TIMESTAMPTZ NOT NULL,
  duration_ms     INTEGER NOT NULL,

  -- v1.5
  notebook_path   TEXT,
  cell_id         TEXT
);

CREATE INDEX ops_ws_time   ON operations (workspace_id, started_at DESC);
CREATE INDEX ops_ws_env    ON operations (workspace_id, env_id, started_at DESC);
CREATE INDEX ops_ws_source ON operations (workspace_id, source, started_at DESC);
```

- [ ] **Step 3: Write failing test for Insert + List**

Create `internal/db/operations_test.go`:

```go
package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOperationsInsertAndList(t *testing.T) {
	d := openTestDB(t) // adopt whatever helper you confirmed in Step 1
	defer d.Close()

	op := Operation{
		ID:            uuid.NewString(),
		WorkspaceID:   "ws-1",
		UserID:        ptr("u-1"),
		Source:        "sdk",
		ThreadID:      ptr("th-1"),
		RequestID:     ptr("rpc-1"),
		EnvID:         "alpha",
		Tool:          "shell",
		Arguments:     json.RawMessage(`{"command":"ls"}`),
		IsError:       false,
		ResultSummary: ptr("ok"),
		StartedAt:     time.Now().UTC().Truncate(time.Microsecond),
		CompletedAt:   time.Now().UTC().Truncate(time.Microsecond),
		DurationMs:    7,
	}
	if err := d.InsertOperation(op); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := d.ListOperations(OperationFilter{WorkspaceID: "ws-1", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.ID != op.ID || got.EnvID != "alpha" || got.Tool != "shell" {
		t.Fatalf("got = %+v", got)
	}
}

func TestOperationsFilterByEnv(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()

	must := func(env string) {
		err := d.InsertOperation(Operation{
			ID: uuid.NewString(), WorkspaceID: "ws-1", Source: "sdk",
			EnvID: env, Tool: "shell", IsError: false,
			Arguments: json.RawMessage(`{}`),
			StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
			DurationMs: 1,
		})
		if err != nil {
			t.Fatalf("insert(%s): %v", env, err)
		}
	}
	must("alpha")
	must("alpha")
	must("beta")

	rows, err := d.ListOperations(OperationFilter{WorkspaceID: "ws-1", EnvID: "alpha", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("alpha rows = %d, want 2", len(rows))
	}
}

func TestOperationsPruneOlderThan(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()

	oldT := time.Now().UTC().Add(-100 * 24 * time.Hour)
	newT := time.Now().UTC()
	for _, st := range []time.Time{oldT, newT} {
		err := d.InsertOperation(Operation{
			ID: uuid.NewString(), WorkspaceID: "ws", Source: "sdk",
			EnvID: "a", Tool: "shell", IsError: false,
			Arguments: json.RawMessage(`{}`),
			StartedAt: st, CompletedAt: st, DurationMs: 1,
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	n, err := d.PruneOperationsOlderThan(time.Now().Add(-90 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
}

func ptr[T any](v T) *T { return &v }
```

If the existing DB tests don't have a helper named `openTestDB`, substitute the name the repo actually uses (e.g. `dbtest.Open(t)`).

- [ ] **Step 4: Run test to verify failure**

Run: `go test ./internal/db -run 'TestOperations' -v`
Expected: FAIL with compilation errors (`Operation`, `InsertOperation`, etc. undefined).

- [ ] **Step 5: Implement operations.go**

Create `internal/db/operations.go`:

```go
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Operation is one row of the operations table — a single completed
// mcpServer/tool/call observed by codex-app-gateway.
type Operation struct {
	ID             string
	WorkspaceID    string
	UserID         *string
	Source         string // "sdk" | "tui" | (v1.5) "llm"
	ThreadID       *string
	RequestID      *string

	EnvID          string
	Tool           string
	Arguments      json.RawMessage // nil if truncated
	ArgumentsMeta  json.RawMessage // {"truncated":true,"size_bytes":N,"sha256":"..."} if so

	IsError        bool
	ResultSummary  *string
	ResultMeta     json.RawMessage

	StartedAt      time.Time
	CompletedAt    time.Time
	DurationMs     int32

	NotebookPath   *string // v1.5
	CellID         *string // v1.5
}

// OperationFilter is the optional filter set for ListOperations.
type OperationFilter struct {
	WorkspaceID string // REQUIRED — server-side enforced
	EnvID       string // optional
	Tool        string // optional
	Source      string // optional
	IsError     *bool  // optional
	Since       *time.Time
	ID          string // optional: exact match (returns 0 or 1 rows)
	Limit       int    // default 100, max 1000
}

func (db *DB) InsertOperation(o Operation) error {
	const q = `
INSERT INTO operations (
  id, workspace_id, user_id, source, thread_id, request_id,
  env_id, tool, arguments, arguments_meta,
  is_error, result_summary, result_meta,
  started_at, completed_at, duration_ms,
  notebook_path, cell_id
) VALUES (
  $1,$2,$3,$4,$5,$6, $7,$8,$9,$10, $11,$12,$13, $14,$15,$16, $17,$18
)`
	_, err := db.Exec(q,
		o.ID, o.WorkspaceID, o.UserID, o.Source, o.ThreadID, o.RequestID,
		o.EnvID, o.Tool, nullableJSON(o.Arguments), nullableJSON(o.ArgumentsMeta),
		o.IsError, o.ResultSummary, nullableJSON(o.ResultMeta),
		o.StartedAt, o.CompletedAt, o.DurationMs,
		o.NotebookPath, o.CellID,
	)
	return err
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

const defaultListLimit = 100
const maxListLimit = 1000

func (db *DB) ListOperations(f OperationFilter) ([]Operation, error) {
	if f.WorkspaceID == "" {
		return nil, fmt.Errorf("ListOperations: WorkspaceID required")
	}
	if f.Limit <= 0 {
		f.Limit = defaultListLimit
	}
	if f.Limit > maxListLimit {
		f.Limit = maxListLimit
	}

	var (
		args  []any
		where []string
	)
	pushArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where = append(where, "workspace_id = "+pushArg(f.WorkspaceID))
	if f.ID != "" {
		where = append(where, "id = "+pushArg(f.ID))
	}
	if f.EnvID != "" {
		where = append(where, "env_id = "+pushArg(f.EnvID))
	}
	if f.Tool != "" {
		where = append(where, "tool = "+pushArg(f.Tool))
	}
	if f.Source != "" {
		where = append(where, "source = "+pushArg(f.Source))
	}
	if f.IsError != nil {
		where = append(where, "is_error = "+pushArg(*f.IsError))
	}
	if f.Since != nil {
		where = append(where, "started_at >= "+pushArg(*f.Since))
	}
	limit := pushArg(f.Limit)

	q := `SELECT id, workspace_id, user_id, source, thread_id, request_id,
		env_id, tool, arguments, arguments_meta,
		is_error, result_summary, result_meta,
		started_at, completed_at, duration_ms,
		notebook_path, cell_id
		FROM operations WHERE ` + strings.Join(where, " AND ") +
		" ORDER BY started_at DESC LIMIT " + limit
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		var o Operation
		var args, argsMeta, resMeta sql.NullString
		err := rows.Scan(
			&o.ID, &o.WorkspaceID, &o.UserID, &o.Source, &o.ThreadID, &o.RequestID,
			&o.EnvID, &o.Tool, &args, &argsMeta,
			&o.IsError, &o.ResultSummary, &resMeta,
			&o.StartedAt, &o.CompletedAt, &o.DurationMs,
			&o.NotebookPath, &o.CellID,
		)
		if err != nil {
			return nil, err
		}
		if args.Valid {
			o.Arguments = json.RawMessage(args.String)
		}
		if argsMeta.Valid {
			o.ArgumentsMeta = json.RawMessage(argsMeta.String)
		}
		if resMeta.Valid {
			o.ResultMeta = json.RawMessage(resMeta.String)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PruneOperationsOlderThan deletes operations whose started_at is strictly
// before cutoff. Returns the number of rows deleted.
func (db *DB) PruneOperationsOlderThan(cutoff time.Time) (int64, error) {
	res, err := db.Exec("DELETE FROM operations WHERE started_at < $1", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./internal/db -run 'TestOperations' -v`
Expected: 3 passed.

- [ ] **Step 7: Commit**

```bash
cd /root/agentserver
git add internal/db/migrations/023_operations.sql \
        internal/db/operations.go \
        internal/db/operations_test.go
git commit -m "feat(db): operations table + insert/list/prune queries

Plan 2 schema for the operation log. Columns include v1.5 nullable
notebook_path/cell_id so we don't need a follow-up migration."
```

---

## Task 2: agentserver `/internal/operations` handlers

**Files:**
- Create: `internal/server/operations.go`
- Create: `internal/server/operations_test.go`
- Modify: `internal/server/server.go` — wire the new routes (find the chi router setup, add `/internal/operations` POST + GET; reuse the existing internal-secret middleware)

- [ ] **Step 1: Inspect how other /internal/* routes are wired**

Read enough of `internal/server/server.go` to find the chi router setup and identify the middleware used for bearer-auth on `/internal/*`. Typical name: `requireInternalSecret` or similar. If unclear, grep:

```bash
grep -rn "AgentserverInternalSecret\|internal.*Bearer\|requireInternal" /root/agentserver/internal/server/ | head -10
```

Adopt whatever pattern is already in use. Don't invent new middleware.

- [ ] **Step 2: Write failing handler tests**

Create `internal/server/operations_test.go`:

```go
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostInternalOperations_WritesRow(t *testing.T) {
	s := newTestServer(t) // reuse whatever existing test helper builds *Server with a real DB
	defer s.Close()

	body := map[string]any{
		"id":            uuid.NewString(),
		"workspace_id":  "ws-1",
		"user_id":       "u-1",
		"source":        "sdk",
		"env_id":        "alpha",
		"tool":          "shell",
		"arguments":     map[string]any{"command": "ls"},
		"is_error":      false,
		"result_summary": "ok",
		"started_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"completed_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"duration_ms":   8,
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/internal/operations", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+s.internalSecretForTests())
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	s.routerForTests().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPostInternalOperations_Unauthorized(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()

	req := httptest.NewRequest(http.MethodPost, "/internal/operations", bytes.NewReader([]byte(`{}`)))
	// no Authorization header
	rr := httptest.NewRecorder()
	s.routerForTests().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rr.Code)
	}
}

func TestGetInternalOperations_FiltersByWorkspaceAndTool(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()

	post := func(wsID, env, tool string) {
		body := map[string]any{
			"id":           uuid.NewString(),
			"workspace_id": wsID,
			"source":       "sdk",
			"env_id":       env,
			"tool":         tool,
			"arguments":    map[string]any{},
			"is_error":     false,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
			"duration_ms":  1,
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/internal/operations", bytes.NewReader(buf))
		req.Header.Set("Authorization", "Bearer "+s.internalSecretForTests())
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.routerForTests().ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("post: %d %s", rr.Code, rr.Body.String())
		}
	}
	post("ws-1", "alpha", "shell")
	post("ws-1", "alpha", "read_file")
	post("ws-2", "alpha", "shell")

	req := httptest.NewRequest(http.MethodGet,
		"/internal/operations?workspace_id=ws-1&tool=shell&limit=50", nil)
	req.Header.Set("Authorization", "Bearer "+s.internalSecretForTests())
	rr := httptest.NewRecorder()
	s.routerForTests().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Operations []map[string]any `json:"operations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Operations) != 1 {
		t.Fatalf("rows = %d, want 1", len(resp.Operations))
	}
	if resp.Operations[0]["workspace_id"] != "ws-1" || resp.Operations[0]["tool"] != "shell" {
		t.Fatalf("row = %+v", resp.Operations[0])
	}
}
```

If `newTestServer` / `routerForTests` / `internalSecretForTests` aren't existing helpers, use the closest equivalents (look at how `handler_tui_internal_test.go` already builds `internalRouter(s)` — pattern is `internalRouter(s).ServeHTTP(...)`).

- [ ] **Step 3: Run to verify failure**

```bash
go test ./internal/server -run 'TestPostInternalOperations|TestGetInternalOperations' -v
```
Expected: FAIL — handler not registered.

- [ ] **Step 4: Implement handlers**

Create `internal/server/operations.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/agentserver/agentserver/internal/db"
)

// postInternalOperations is POST /internal/operations.
// Body is a JSON object matching the agentserver "operation record" shape.
// Auth: AgentserverInternalSecret bearer (via existing internal middleware).
func (s *Server) postInternalOperations(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID            string          `json:"id"`
		WorkspaceID   string          `json:"workspace_id"`
		UserID        *string         `json:"user_id,omitempty"`
		Source        string          `json:"source"`
		ThreadID      *string         `json:"thread_id,omitempty"`
		RequestID     *string         `json:"request_id,omitempty"`
		EnvID         string          `json:"env_id"`
		Tool          string          `json:"tool"`
		Arguments     json.RawMessage `json:"arguments,omitempty"`
		ArgumentsMeta json.RawMessage `json:"arguments_meta,omitempty"`
		IsError       bool            `json:"is_error"`
		ResultSummary *string         `json:"result_summary,omitempty"`
		ResultMeta    json.RawMessage `json:"result_meta,omitempty"`
		StartedAt     time.Time       `json:"started_at"`
		CompletedAt   time.Time       `json:"completed_at"`
		DurationMs    int32           `json:"duration_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.ID == "" || body.WorkspaceID == "" || body.Source == "" || body.EnvID == "" || body.Tool == "" {
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}
	op := db.Operation{
		ID: body.ID, WorkspaceID: body.WorkspaceID, UserID: body.UserID,
		Source: body.Source, ThreadID: body.ThreadID, RequestID: body.RequestID,
		EnvID: body.EnvID, Tool: body.Tool,
		Arguments: body.Arguments, ArgumentsMeta: body.ArgumentsMeta,
		IsError: body.IsError, ResultSummary: body.ResultSummary, ResultMeta: body.ResultMeta,
		StartedAt: body.StartedAt, CompletedAt: body.CompletedAt, DurationMs: body.DurationMs,
	}
	if err := s.db.InsertOperation(op); err != nil {
		s.log.Warn("operations: insert failed", "err", err, "workspace_id", op.WorkspaceID)
		http.Error(w, "insert failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getInternalOperations is GET /internal/operations.
// Query params: workspace_id (required), env_id, tool, source, is_error, since
// (RFC3339), id, limit (default 100, max 1000).
func (s *Server) getInternalOperations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	wsID := q.Get("workspace_id")
	if wsID == "" {
		http.Error(w, "workspace_id required", http.StatusBadRequest)
		return
	}
	f := db.OperationFilter{
		WorkspaceID: wsID,
		EnvID:       q.Get("env_id"),
		Tool:        q.Get("tool"),
		Source:      q.Get("source"),
		ID:          q.Get("id"),
	}
	if v := q.Get("is_error"); v != "" {
		b := v == "true" || v == "1"
		f.IsError = &b
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			http.Error(w, "since: "+err.Error(), http.StatusBadRequest)
			return
		}
		f.Since = &t
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "limit: invalid", http.StatusBadRequest)
			return
		}
		f.Limit = n
	}
	rows, err := s.db.ListOperations(f)
	if err != nil {
		s.log.Warn("operations: list failed", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	type respRow struct {
		ID            string          `json:"id"`
		WorkspaceID   string          `json:"workspace_id"`
		UserID        *string         `json:"user_id,omitempty"`
		Source        string          `json:"source"`
		ThreadID      *string         `json:"thread_id,omitempty"`
		EnvID         string          `json:"env_id"`
		Tool          string          `json:"tool"`
		Arguments     json.RawMessage `json:"arguments,omitempty"`
		ArgumentsMeta json.RawMessage `json:"arguments_meta,omitempty"`
		IsError       bool            `json:"is_error"`
		ResultSummary *string         `json:"result_summary,omitempty"`
		ResultMeta    json.RawMessage `json:"result_meta,omitempty"`
		StartedAt     time.Time       `json:"started_at"`
		CompletedAt   time.Time       `json:"completed_at"`
		DurationMs    int32           `json:"duration_ms"`
	}
	out := make([]respRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, respRow{
			ID: r.ID, WorkspaceID: r.WorkspaceID, UserID: r.UserID,
			Source: r.Source, ThreadID: r.ThreadID,
			EnvID: r.EnvID, Tool: r.Tool,
			Arguments: r.Arguments, ArgumentsMeta: r.ArgumentsMeta,
			IsError: r.IsError, ResultSummary: r.ResultSummary, ResultMeta: r.ResultMeta,
			StartedAt: r.StartedAt, CompletedAt: r.CompletedAt, DurationMs: r.DurationMs,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"operations": out})
}
```

- [ ] **Step 5: Wire routes in `server.go`**

Find where `internal/*` routes are mounted on the chi router. Add:

```go
r.Post("/internal/operations", s.postInternalOperations)
r.Get("/internal/operations",  s.getInternalOperations)
```

Both should sit behind the existing internal-secret middleware.

- [ ] **Step 6: Run tests, confirm pass**

```bash
go test ./internal/server -run 'TestPostInternalOperations|TestGetInternalOperations' -v
```
Expected: 3 passed.

- [ ] **Step 7: Commit**

```bash
cd /root/agentserver
git add internal/server/operations.go internal/server/operations_test.go internal/server/server.go
git commit -m "feat(server): /internal/operations POST + GET handlers

bearer-secret authed; POST is fire-and-forget for the gateway; GET
supports workspace_id+env/tool/source/is_error/since/id+limit filters."
```

---

## Task 3: agentserver retention goroutine

**Files:**
- Create: `internal/server/operations_retention.go`
- Create: `internal/server/operations_retention_test.go`
- Modify: `internal/server/server.go` — start retention loop in server boot; cancel on shutdown
- Modify: `main.go` — pass retention config (TTL + interval) through

- [ ] **Step 1: Write failing test**

Create `internal/server/operations_retention_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/agentserver/agentserver/internal/db"
)

func TestStartRetention_DeletesOldRows(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()

	// Seed old + new
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	now := time.Now().UTC()
	for _, ts := range []time.Time{old, now} {
		err := s.db.InsertOperation(db.Operation{
			ID: uuid.NewString(), WorkspaceID: "ws", Source: "sdk",
			EnvID: "a", Tool: "shell", IsError: false,
			Arguments: json.RawMessage(`{}`),
			StartedAt: ts, CompletedAt: ts, DurationMs: 1,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Run a single iteration explicitly (test exposes the inner step)
	n, err := s.runRetentionOnce(90 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}

	// Confirm the new row survived
	rows, err := s.db.ListOperations(db.OperationFilter{WorkspaceID: "ws", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("remaining = %d, want 1", len(rows))
	}

	_ = ctx
}

func TestStartRetention_TickerStops(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.startRetentionLoop(ctx, 90*24*time.Hour, 50*time.Millisecond)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention loop did not exit after ctx cancel")
	}
}
```

- [ ] **Step 2: Run to confirm failure**

```bash
go test ./internal/server -run 'TestStartRetention' -v
```
Expected: FAIL — `runRetentionOnce` / `startRetentionLoop` undefined.

- [ ] **Step 3: Implement retention loop**

Create `internal/server/operations_retention.go`:

```go
package server

import (
	"context"
	"time"
)

// runRetentionOnce deletes operations older than ttl. Exposed for tests.
func (s *Server) runRetentionOnce(ttl time.Duration) (int64, error) {
	cutoff := time.Now().Add(-ttl)
	return s.db.PruneOperationsOlderThan(cutoff)
}

// startRetentionLoop ticks every `every` and prunes operations older
// than ttl. Returns when ctx is cancelled. Errors are logged, not
// propagated — a transient PG failure shouldn't kill the loop.
func (s *Server) startRetentionLoop(ctx context.Context, ttl time.Duration, every time.Duration) {
	if every <= 0 {
		every = time.Hour
	}
	if ttl <= 0 {
		return // disabled
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.runRetentionOnce(ttl)
			if err != nil {
				s.log.Warn("operations retention: prune failed", "err", err)
				continue
			}
			if n > 0 {
				s.log.Info("operations retention: pruned", "rows", n)
			}
		}
	}
}
```

- [ ] **Step 4: Wire startup in server.go**

In `server.go` (or wherever the server `Run`/`Serve` lifecycle lives), launch the retention goroutine bound to the server's lifetime context with the configured TTL + interval. Default TTL: 90 days. Default interval: 1 hour. If `ttl == 0`, skip the loop (retention disabled).

Read the surrounding code first to choose the right insertion point — likely near where other background goroutines are started (look for `go s.Something`).

- [ ] **Step 5: Run tests, confirm pass**

```bash
go test ./internal/server -run 'TestStartRetention' -v
```
Expected: 2 passed.

- [ ] **Step 6: Commit**

```bash
cd /root/agentserver
git add internal/server/operations_retention.go \
        internal/server/operations_retention_test.go \
        internal/server/server.go
git commit -m "feat(server): hourly operations retention loop

prunes operations older than configured TTL (default 90 days). Loop
exits cleanly on context cancel; transient errors logged not fatal."
```

---

## Task 4: `oplog.Client` — async HTTP submitter

**Files:**
- Create: `internal/codexappgateway/oplog/doc.go`
- Create: `internal/codexappgateway/oplog/client.go`
- Create: `internal/codexappgateway/oplog/client_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/codexappgateway/oplog/client_test.go`:

```go
package oplog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_Submit_FireAndForgetPOST(t *testing.T) {
	var (
		mu      sync.Mutex
		bodies  [][]byte
		gotAuth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		mu.Lock()
		bodies = append(bodies, buf)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/internal/operations", "secret-x", 16)
	defer c.Close()

	op := Operation{
		ID: "op-1", WorkspaceID: "ws", Source: "sdk",
		EnvID: "alpha", Tool: "shell", IsError: false,
		StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
		DurationMs: 1,
	}
	c.Submit(op) // non-blocking
	c.Submit(op)

	// Wait for the background drainer to flush
	if !waitFor(2*time.Second, func() bool {
		mu.Lock(); defer mu.Unlock()
		return len(bodies) == 2
	}) {
		t.Fatalf("flushed %d, want 2", len(bodies))
	}
	if gotAuth != "Bearer secret-x" {
		t.Fatalf("auth header = %q", gotAuth)
	}
}

func TestClient_Submit_BoundedDropsOnFull(t *testing.T) {
	// Stall the server forever; channel fills, Submit must NOT block.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	c := NewClient(srv.URL+"/", "s", 2) // tiny channel
	defer c.Close()

	op := Operation{
		ID: "op", WorkspaceID: "ws", Source: "sdk",
		EnvID: "a", Tool: "shell",
		StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
	}
	// Fire many; should not block even when channel is full.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			c.Submit(op)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Submit blocked")
	}
	if c.Dropped() == 0 {
		t.Fatalf("expected drops, got %d", c.Dropped())
	}
}

func TestClient_Submit_DoesNotBlockOnServerError(t *testing.T) {
	hits := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "s", 16)
	defer c.Close()

	op := Operation{
		ID: "x", WorkspaceID: "ws", Source: "sdk",
		EnvID: "a", Tool: "shell",
		StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
	}
	c.Submit(op)
	if !waitFor(time.Second, func() bool { return atomic.LoadInt64(&hits) >= 1 }) {
		t.Fatal("server never received the post")
	}
	// Server returned 500; client must keep accepting more submits.
	c.Submit(op)
	if !waitFor(time.Second, func() bool { return atomic.LoadInt64(&hits) >= 2 }) {
		t.Fatal("client stopped sending after server error")
	}
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestOperation_MarshalJSON(t *testing.T) {
	o := Operation{
		ID: "x", WorkspaceID: "w", Source: "sdk",
		EnvID: "a", Tool: "shell", IsError: false,
		Arguments: json.RawMessage(`{"a":1}`),
		StartedAt: time.Unix(1700000000, 0).UTC(),
		CompletedAt: time.Unix(1700000001, 0).UTC(),
		DurationMs: 5,
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !contains(b, `"workspace_id":"w"`) || !contains(b, `"arguments":{"a":1}`) {
		t.Fatalf("body = %s", b)
	}
}

func contains(b []byte, s string) bool { return string(b)[:0] == "" && bytesIndex(b, s) >= 0 }
func bytesIndex(b []byte, s string) int {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run, confirm failure**

```bash
go test ./internal/codexappgateway/oplog/ -v
```
Expected: FAIL — package missing.

- [ ] **Step 3: Implement package**

Create `internal/codexappgateway/oplog/doc.go`:

```go
// Package oplog publishes per-call operation records from codex-app-gateway
// to agentserver's /internal/operations POST endpoint. Submit is async and
// fire-and-forget; tool calls never block on log delivery.
package oplog
```

Create `internal/codexappgateway/oplog/client.go`:

```go
package oplog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Operation is what we POST to agentserver /internal/operations.
// Field shapes match internal/server/operations.go's request struct.
type Operation struct {
	ID            string          `json:"id"`
	WorkspaceID   string          `json:"workspace_id"`
	UserID        *string         `json:"user_id,omitempty"`
	Source        string          `json:"source"`
	ThreadID      *string         `json:"thread_id,omitempty"`
	RequestID     *string         `json:"request_id,omitempty"`

	EnvID         string          `json:"env_id"`
	Tool          string          `json:"tool"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	ArgumentsMeta json.RawMessage `json:"arguments_meta,omitempty"`

	IsError       bool            `json:"is_error"`
	ResultSummary *string         `json:"result_summary,omitempty"`
	ResultMeta    json.RawMessage `json:"result_meta,omitempty"`

	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   time.Time       `json:"completed_at"`
	DurationMs    int32           `json:"duration_ms"`
}

// Client posts Operations to agentserver. Submit is non-blocking; one
// background goroutine drains a bounded channel and POSTs each one.
type Client struct {
	url     string
	secret  string
	ch      chan Operation
	hc      *http.Client
	logger  *slog.Logger
	dropped uint64
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewClient starts the background drainer immediately. Capacity bounds
// how many Operations can queue before Submit starts dropping.
func NewClient(url, secret string, capacity int) *Client {
	if capacity <= 0 {
		capacity = 1024
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		url:    url,
		secret: secret,
		ch:     make(chan Operation, capacity),
		hc:     &http.Client{Timeout: 5 * time.Second},
		logger: slog.Default().With("component", "oplog"),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go c.drain(ctx)
	return c
}

// Submit enqueues op. Never blocks. Drops on full channel and bumps the
// `dropped` counter, which a metrics exporter can read via Dropped().
func (c *Client) Submit(op Operation) {
	select {
	case c.ch <- op:
	default:
		atomic.AddUint64(&c.dropped, 1)
	}
}

// Dropped is the cumulative number of Submit calls that hit a full channel.
func (c *Client) Dropped() uint64 { return atomic.LoadUint64(&c.dropped) }

// Close stops the drainer. Already-queued ops are abandoned.
func (c *Client) Close() {
	c.cancel()
	<-c.done
}

func (c *Client) drain(ctx context.Context) {
	defer close(c.done)
	for {
		select {
		case <-ctx.Done():
			return
		case op := <-c.ch:
			c.post(ctx, op)
		}
	}
}

func (c *Client) post(ctx context.Context, op Operation) {
	buf, err := json.Marshal(op)
	if err != nil {
		c.logger.Warn("oplog: marshal failed", "id", op.ID, "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(buf))
	if err != nil {
		c.logger.Warn("oplog: build request failed", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		c.logger.Warn("oplog: POST failed", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.logger.Warn("oplog: agentserver non-2xx",
			"status", resp.StatusCode, "body", string(body))
	}
}

// ListClient is the synchronous read-side: used by the gateway's
// operations/list RPC interceptor. Separate type so the async/sync
// surfaces don't share channels.
type ListClient struct {
	url    string
	secret string
	hc     *http.Client
}

func NewListClient(url, secret string) *ListClient {
	return &ListClient{
		url: url, secret: secret,
		hc: &http.Client{Timeout: 10 * time.Second},
	}
}

// List forwards filter params to agentserver and returns the decoded
// `operations` slice (raw, so the gateway can re-encode into the ws frame).
func (c *ListClient) List(ctx context.Context, params map[string]string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("operations list: %d %s", resp.StatusCode, string(body))
	}
	return body, nil
}
```

The test file uses some helpers (`contains`, `bytesIndex`) that I inlined to avoid an extra import. Feel free to replace with `strings.Contains(string(b), s)` after the test passes — equivalent.

- [ ] **Step 4: Run tests, confirm pass**

```bash
go test ./internal/codexappgateway/oplog/ -v
```
Expected: 4 passed.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/codexappgateway/oplog/
git commit -m "feat(oplog): async HTTP client + sync ListClient

Submit is fire-and-forget over bounded channel (default 1024); drops
counted. Sync ListClient is used by the gateway's operations/list
interceptor (Task 6)."
```

---

## Task 5: `oplog.Interceptor` — JSON-RPC frame parsing

**Files:**
- Create: `internal/codexappgateway/oplog/interceptor.go`
- Create: `internal/codexappgateway/oplog/interceptor_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/codexappgateway/oplog/interceptor_test.go`:

```go
package oplog

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type captureClient struct {
	mu  sync.Mutex
	ops []Operation
}

func (c *captureClient) Submit(op Operation) {
	c.mu.Lock(); defer c.mu.Unlock()
	c.ops = append(c.ops, op)
}
func (c *captureClient) Dropped() uint64 { return 0 }

// Pairs request + response into one Operation.
func TestInterceptor_ParesRequestAndResponse(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws-1"})

	req := []byte(`{"jsonrpc":"2.0","id":7,"method":"mcpServer/tool/call","params":{
		"thread_id":"th-1","server":"env_mcp","tool":"shell",
		"arguments":{"environment_id":"alpha","command":"ls"},
		"_meta":{"agentserver_user_id":"u-1"}}}`)
	resp := []byte(`{"jsonrpc":"2.0","id":7,"result":{
		"content":[{"type":"text","text":"hi"}],"isError":false}}`)

	i.OnClientFrame(req)
	i.OnServerFrame(resp)

	if len(cc.ops) != 1 {
		t.Fatalf("ops = %d", len(cc.ops))
	}
	op := cc.ops[0]
	if op.WorkspaceID != "ws-1" || op.Source != "sdk" ||
		op.EnvID != "alpha" || op.Tool != "shell" || op.IsError {
		t.Fatalf("op = %+v", op)
	}
	if op.UserID == nil || *op.UserID != "u-1" {
		t.Fatalf("user_id = %v", op.UserID)
	}
	if op.ThreadID == nil || *op.ThreadID != "th-1" {
		t.Fatalf("thread_id = %v", op.ThreadID)
	}
	if op.ResultSummary == nil || *op.ResultSummary != "hi" {
		t.Fatalf("result_summary = %v", op.ResultSummary)
	}
}

// Frames that aren't tool/call (other methods, notifications, malformed) pass
// without being logged.
func TestInterceptor_IgnoresIrrelevantFrames(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws-1"})

	i.OnClientFrame([]byte(`{"method":"initialized"}`)) // notification, no id
	i.OnClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{}}`))
	i.OnServerFrame([]byte(`{"jsonrpc":"2.0","id":1,"result":{"thread_id":"x"}}`))
	i.OnServerFrame([]byte(`not json`))

	if len(cc.ops) != 0 {
		t.Fatalf("logged unrelated frames: %+v", cc.ops)
	}
}

// isError=true is captured.
func TestInterceptor_RecordsIsError(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws"})
	i.OnClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"mcpServer/tool/call","params":{
		"thread_id":"t","server":"env_mcp","tool":"shell",
		"arguments":{"environment_id":"a","command":"bad"}}}`))
	i.OnServerFrame([]byte(`{"jsonrpc":"2.0","id":3,"result":{
		"content":[{"type":"text","text":"oops"}],"isError":true}}`))

	if len(cc.ops) != 1 || !cc.ops[0].IsError {
		t.Fatalf("ops = %+v", cc.ops)
	}
}

// Arguments larger than the threshold are replaced with arguments_meta.
func TestInterceptor_TruncatesLargeArguments(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws", ArgsMaxBytes: 50})

	big := make([]byte, 200)
	for x := range big {
		big[x] = 'a'
	}
	args, _ := json.Marshal(map[string]any{"data": string(big), "environment_id": "a"})
	frame := append([]byte(`{"jsonrpc":"2.0","id":11,"method":"mcpServer/tool/call","params":{
		"server":"env_mcp","tool":"write_file","arguments":`), args...)
	frame = append(frame, '}', '}')
	i.OnClientFrame(frame)
	i.OnServerFrame([]byte(`{"jsonrpc":"2.0","id":11,"result":{"content":[],"isError":false}}`))

	if len(cc.ops) != 1 {
		t.Fatalf("ops = %d", len(cc.ops))
	}
	op := cc.ops[0]
	if op.Arguments != nil {
		t.Fatal("arguments should be nil when truncated")
	}
	if op.ArgumentsMeta == nil {
		t.Fatal("arguments_meta missing")
	}
	var m map[string]any
	_ = json.Unmarshal(op.ArgumentsMeta, &m)
	if m["truncated"] != true {
		t.Fatalf("arguments_meta = %v", m)
	}
}

// Long text results are truncated; result_meta records total size.
func TestInterceptor_TruncatesLargeTextResult(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws", ResultMaxBytes: 10})

	long := make([]byte, 100)
	for x := range long {
		long[x] = 'z'
	}
	i.OnClientFrame([]byte(`{"jsonrpc":"2.0","id":4,"method":"mcpServer/tool/call","params":{
		"server":"env_mcp","tool":"read_file","arguments":{"environment_id":"a","path":"/x"}}}`))
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 4,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(long)}},
			"isError": false,
		},
	})
	i.OnServerFrame(resp)

	op := cc.ops[0]
	if op.ResultSummary == nil || len(*op.ResultSummary) > 10 {
		t.Fatalf("result_summary = %v", op.ResultSummary)
	}
	if op.ResultMeta == nil {
		t.Fatal("result_meta missing")
	}
}

// Concurrent request/response handling — id-keyed pending map.
func TestInterceptor_ConcurrentPairs(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws"})

	var wg sync.WaitGroup
	var sent int64
	for n := 0; n < 50; n++ {
		wg.Add(1)
		id := n + 1
		go func() {
			defer wg.Done()
			req := []byte(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"mcpServer/tool/call","params":{
				"server":"env_mcp","tool":"shell","arguments":{"environment_id":"a","command":"x"}}}`)
			resp := []byte(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"result":{"content":[],"isError":false}}`)
			i.OnClientFrame(req)
			i.OnServerFrame(resp)
			atomic.AddInt64(&sent, 1)
		}()
	}
	wg.Wait()
	if !waitFor(2*time.Second, func() bool {
		cc.mu.Lock(); defer cc.mu.Unlock()
		return len(cc.ops) == 50
	}) {
		t.Fatalf("ops = %d, want 50", len(cc.ops))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
```

- [ ] **Step 2: Run, confirm failure**

```bash
go test ./internal/codexappgateway/oplog/ -run TestInterceptor -v
```
Expected: FAIL — `Interceptor`, `NewInterceptor`, `Config` undefined.

- [ ] **Step 3: Implement Interceptor**

Create `internal/codexappgateway/oplog/interceptor.go`:

```go
package oplog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultArgsMaxBytes   = 64 * 1024
	defaultResultMaxBytes = 4 * 1024
)

// Submitter is the subset of Client that the Interceptor needs. Lets
// tests pass a capture stub.
type Submitter interface {
	Submit(Operation)
}

// Config tunes the Interceptor at construction.
type Config struct {
	Source         string // "sdk" | "tui"
	WorkspaceID    string // pinned from the ws Identity
	ArgsMaxBytes   int    // 0 → 64 KiB
	ResultMaxBytes int    // 0 → 4 KiB
}

// Interceptor parses JSON-RPC frames as they cross the ws bridge. On a
// matched request+response for mcpServer/tool/call, it emits one
// Operation to the Submitter.
type Interceptor struct {
	sub Submitter
	cfg Config

	mu      sync.Mutex
	pending map[any]pendingReq
}

type pendingReq struct {
	startedAt time.Time
	env       string
	tool      string
	threadID  string
	userID    string
	args      json.RawMessage
	argsSize  int
}

func NewInterceptor(s Submitter, cfg Config) *Interceptor {
	if cfg.ArgsMaxBytes <= 0 {
		cfg.ArgsMaxBytes = defaultArgsMaxBytes
	}
	if cfg.ResultMaxBytes <= 0 {
		cfg.ResultMaxBytes = defaultResultMaxBytes
	}
	return &Interceptor{sub: s, cfg: cfg, pending: map[any]pendingReq{}}
}

// OnClientFrame is called on every client→server frame BEFORE it's forwarded.
// Never blocks. Errors are silent — parsing failures are not Interceptor's
// problem; the underlying pump still forwards the bytes.
func (i *Interceptor) OnClientFrame(frame []byte) {
	var msg struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		return
	}
	if msg.ID == nil || msg.Method != "mcpServer/tool/call" {
		return
	}
	var p struct {
		ThreadID  string          `json:"thread_id"`
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			UserID string `json:"agentserver_user_id"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return
	}
	var envID string
	if len(p.Arguments) > 0 {
		var args struct {
			EnvironmentID string `json:"environment_id"`
		}
		_ = json.Unmarshal(p.Arguments, &args)
		envID = args.EnvironmentID
	}

	i.mu.Lock()
	i.pending[msg.ID] = pendingReq{
		startedAt: time.Now().UTC(),
		env:       envID,
		tool:      p.Tool,
		threadID:  p.ThreadID,
		userID:    p.Meta.UserID,
		args:      p.Arguments,
		argsSize:  len(p.Arguments),
	}
	i.mu.Unlock()
}

// OnServerFrame is called on every server→client frame. If it pairs with
// a pending request, emits an Operation.
func (i *Interceptor) OnServerFrame(frame []byte) {
	var msg struct {
		ID     any             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		return
	}
	if msg.ID == nil {
		return
	}
	i.mu.Lock()
	pr, ok := i.pending[msg.ID]
	if ok {
		delete(i.pending, msg.ID)
	}
	i.mu.Unlock()
	if !ok {
		return
	}

	op := Operation{
		ID:          uuid.NewString(),
		WorkspaceID: i.cfg.WorkspaceID,
		Source:      i.cfg.Source,
		EnvID:       pr.env,
		Tool:        pr.tool,
		StartedAt:   pr.startedAt,
		CompletedAt: time.Now().UTC(),
	}
	if pr.threadID != "" {
		t := pr.threadID
		op.ThreadID = &t
	}
	if pr.userID != "" {
		u := pr.userID
		op.UserID = &u
	}
	op.DurationMs = int32(op.CompletedAt.Sub(op.StartedAt) / time.Millisecond)

	// Arguments — truncate if oversized
	if pr.argsSize > i.cfg.ArgsMaxBytes {
		sum := sha256.Sum256(pr.args)
		op.ArgumentsMeta = mustJSON(map[string]any{
			"truncated":   true,
			"size_bytes":  pr.argsSize,
			"sha256":      hex.EncodeToString(sum[:]),
		})
	} else {
		op.Arguments = pr.args
	}

	// Result — pull text content as result_summary; record error flag
	if len(msg.Error) > 0 {
		op.IsError = true
		// Use the rpc error message as result_summary
		var er struct{ Message string `json:"message"` }
		_ = json.Unmarshal(msg.Error, &er)
		s := er.Message
		op.ResultSummary = &s
	} else if len(msg.Result) > 0 {
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(msg.Result, &r)
		op.IsError = r.IsError
		var b []byte
		for _, c := range r.Content {
			if c.Type == "text" {
				b = append(b, c.Text...)
			}
		}
		total := len(b)
		if total > i.cfg.ResultMaxBytes {
			truncated := string(b[:i.cfg.ResultMaxBytes])
			op.ResultSummary = &truncated
			op.ResultMeta = mustJSON(map[string]any{
				"truncated":   true,
				"total_bytes": total,
			})
		} else if total > 0 {
			s := string(b)
			op.ResultSummary = &s
		}
	}

	i.sub.Submit(op)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
```

- [ ] **Step 4: Run tests, confirm pass**

```bash
go test ./internal/codexappgateway/oplog/ -v
```
Expected: 10 passed (4 client + 6 interceptor).

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/codexappgateway/oplog/interceptor.go \
        internal/codexappgateway/oplog/interceptor_test.go
git commit -m "feat(oplog): JSON-RPC frame Interceptor

pending-map pairs request+response by JSON-RPC id; truncation of
oversized args/results; UTF-8 text content collected as result_summary.
Non-tool/call frames pass through untouched (never breaks the pump)."
```

---

## Task 6: `oplog.operations/list` SDK request handler

**Files:**
- Create: `internal/codexappgateway/oplog/operations_list.go`
- Create: `internal/codexappgateway/oplog/operations_list_test.go`

- [ ] **Step 1: Write failing test**

Create `internal/codexappgateway/oplog/operations_list_test.go`:

```go
package oplog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleOperationsList_ForwardsAndReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("workspace_id") != "ws-1" {
			t.Fatalf("ws=%q", q.Get("workspace_id"))
		}
		if q.Get("limit") != "10" {
			t.Fatalf("limit=%q", q.Get("limit"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"operations": []map[string]any{
				{"id": "op_1", "env_id": "a", "tool": "shell", "is_error": false},
			},
		})
	}))
	defer srv.Close()

	lc := NewListClient(srv.URL+"/", "s")
	frame, ok := TryHandleOperationsList(context.Background(), lc, "ws-1",
		[]byte(`{"jsonrpc":"2.0","id":42,"method":"operations/list","params":{"limit":10}}`))
	if !ok {
		t.Fatal("not handled")
	}
	var resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 42 {
		t.Fatalf("id=%d", resp.ID)
	}
	if !contains(resp.Result, `"op_1"`) {
		t.Fatalf("result=%s", resp.Result)
	}
}

func TestTryHandleOperationsList_OtherMethodsIgnored(t *testing.T) {
	frame, ok := TryHandleOperationsList(context.Background(), nil, "ws",
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"mcpServer/tool/call"}`))
	if ok || frame != nil {
		t.Fatalf("should not handle")
	}
}

func TestTryHandleOperationsList_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	lc := NewListClient(srv.URL+"/", "s")

	frame, ok := TryHandleOperationsList(context.Background(), lc, "ws",
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"operations/list","params":{}}`))
	if !ok {
		t.Fatal("should handle, but produce error response")
	}
	var resp struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 7 || resp.Error.Code == 0 {
		t.Fatalf("resp = %+v", resp)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

```bash
go test ./internal/codexappgateway/oplog/ -run OperationsList -v
```
Expected: FAIL — `TryHandleOperationsList` undefined.

- [ ] **Step 3: Implement**

Create `internal/codexappgateway/oplog/operations_list.go`:

```go
package oplog

import (
	"context"
	"encoding/json"
	"fmt"
)

// TryHandleOperationsList inspects a client→server frame. If it's an
// `operations/list` JSON-RPC request, it forwards the filters to
// agentserver via the ListClient and returns a complete JSON-RPC response
// frame to send back to the client (ok=true). For any other frame,
// returns (nil, false) — caller forwards the frame normally.
//
// Designed to be called from the gateway's outbound proxy path so the
// request never reaches the codex app-server (which has no operations/list
// method).
func TryHandleOperationsList(
	ctx context.Context,
	lc *ListClient,
	workspaceID string,
	frame []byte,
) ([]byte, bool) {
	var msg struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		return nil, false
	}
	if msg.Method != "operations/list" || msg.ID == nil {
		return nil, false
	}

	params := map[string]string{"workspace_id": workspaceID}
	var p struct {
		Limit   *int    `json:"limit"`
		EnvID   *string `json:"env_id"`
		Tool    *string `json:"tool"`
		Source  *string `json:"source"`
		IsError *bool   `json:"is_error"`
		Since   *string `json:"since"`
		ID      *string `json:"id"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if p.Limit != nil {
		params["limit"] = fmt.Sprintf("%d", *p.Limit)
	}
	if p.EnvID != nil {
		params["env_id"] = *p.EnvID
	}
	if p.Tool != nil {
		params["tool"] = *p.Tool
	}
	if p.Source != nil {
		params["source"] = *p.Source
	}
	if p.IsError != nil {
		params["is_error"] = fmt.Sprintf("%t", *p.IsError)
	}
	if p.Since != nil {
		params["since"] = *p.Since
	}
	if p.ID != nil {
		params["id"] = *p.ID
	}

	body, err := lc.List(ctx, params)
	if err != nil {
		errResp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": msg.ID,
			"error": map[string]any{"code": -32603, "message": err.Error()},
		})
		return errResp, true
	}
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": msg.ID,
		"result": json.RawMessage(body),
	})
	return resp, true
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
go test ./internal/codexappgateway/oplog/ -v
```
Expected: 13 passed.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/codexappgateway/oplog/operations_list.go \
        internal/codexappgateway/oplog/operations_list_test.go
git commit -m "feat(oplog): operations/list SDK request handler

intercepts operations/list before it reaches codex app-server,
delegates to agentserver, returns a JSON-RPC response frame for the
gateway to write back on the same ws."
```

---

## Task 7: `wsbridge.RunProxyWithInterceptor`

**Files:**
- Modify: `internal/wsbridge/wsbridge.go`
- Modify: `internal/wsbridge/wsbridge_test.go`

- [ ] **Step 1: Write failing test**

Append to `internal/wsbridge/wsbridge_test.go`:

```go
func TestRunProxyWithInterceptor_CallbacksAndRewrite(t *testing.T) {
	// a := client-facing; b := server-facing
	a, b := newWSPair(t) // adopt whatever helper the existing tests use
	defer a.Close(websocket.StatusNormalClosure, "")
	defer b.Close(websocket.StatusNormalClosure, "")

	var (
		ctc, stc [][]byte
		mu       sync.Mutex
		rewrite  = []byte(`{"intercepted":true}`)
	)
	intc := wsbridge.Interceptor{
		OnClientFrame: func(frame []byte) []byte {
			mu.Lock(); defer mu.Unlock()
			ctc = append(ctc, append([]byte(nil), frame...))
			return nil // forward unchanged
		},
		OnServerFrame: func(frame []byte) []byte {
			mu.Lock(); defer mu.Unlock()
			stc = append(stc, append([]byte(nil), frame...))
			if string(frame) == "swap-me" {
				return rewrite
			}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wsbridge.RunProxyWithInterceptor(ctx, a, b, intc, nil)

	// Send a frame from client to server, verify echoed
	if err := writeText(a, "hello-server"); err != nil { t.Fatal(err) }
	if got := readText(t, b); got != "hello-server" {
		t.Fatalf("server got %q", got)
	}

	// Send a frame from server to client; intercept rewrites
	if err := writeText(b, "swap-me"); err != nil { t.Fatal(err) }
	if got := readText(t, a); got != `{"intercepted":true}` {
		t.Fatalf("client got %q", got)
	}

	mu.Lock(); defer mu.Unlock()
	if len(ctc) != 1 || string(ctc[0]) != "hello-server" {
		t.Fatalf("ctc=%q", ctc)
	}
	if len(stc) != 1 || string(stc[0]) != "swap-me" {
		t.Fatalf("stc=%q", stc)
	}
}
```

The helpers `newWSPair`, `writeText`, `readText` are already in `wsbridge_test.go` — verify before using.

- [ ] **Step 2: Run, confirm failure**

```bash
go test ./internal/wsbridge -run TestRunProxyWithInterceptor -v
```
Expected: FAIL — `RunProxyWithInterceptor` / `Interceptor` undefined.

- [ ] **Step 3: Add the new type + function**

Append to `internal/wsbridge/wsbridge.go`:

```go
// Interceptor lets a caller observe and optionally rewrite frames as
// they cross the bridge. Both callbacks may be nil. Returning a non-nil
// slice replaces the frame written downstream; returning nil forwards
// the original frame untouched. Callbacks MUST NOT block.
type Interceptor struct {
	OnClientFrame func(frame []byte) []byte // a → b direction
	OnServerFrame func(frame []byte) []byte // b → a direction
}

// RunProxyWithInterceptor is like RunProxy but lets the caller observe
// and rewrite frames. `onFrame` is invoked on every successfully forwarded
// frame (pass nil to skip).
//
// `intc.OnClientFrame` runs in the a→b direction; `OnServerFrame` runs
// in the b→a direction. Returning nil from either callback forwards the
// original frame unchanged; returning a non-nil slice replaces it.
func RunProxyWithInterceptor(ctx context.Context, a, b *websocket.Conn, intc Interceptor, onFrame func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- pumpWithIntercept(ctx, a, b, intc.OnClientFrame, onFrame) }()
	go func() { errCh <- pumpWithIntercept(ctx, b, a, intc.OnServerFrame, onFrame) }()
	go keepAlive(ctx, a)
	err := <-errCh
	cancel()
	<-errCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func pumpWithIntercept(
	ctx context.Context,
	src, dst *websocket.Conn,
	onFrameBytes func([]byte) []byte,
	onTick func(),
) error {
	for {
		mt, data, err := src.Read(ctx)
		if err != nil {
			closeErr := websocket.CloseStatus(err)
			if closeErr == websocket.StatusNormalClosure || closeErr == websocket.StatusGoingAway {
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		out := data
		if onFrameBytes != nil {
			if rewritten := onFrameBytes(data); rewritten != nil {
				out = rewritten
			}
		}
		if onTick != nil {
			onTick()
		}
		if err := dst.Write(ctx, mt, out); err != nil {
			return err
		}
	}
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
go test ./internal/wsbridge -v
```
Expected: all existing + new test pass.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/wsbridge/wsbridge.go internal/wsbridge/wsbridge_test.go
git commit -m "feat(wsbridge): RunProxyWithInterceptor

callbacks observe + may rewrite frames per direction. Returning nil
forwards untouched. Existing RunProxy behaviour unchanged."
```

---

## Task 8: Gateway `/notebook/ws` route + Interceptor wiring

**Files:**
- Modify: `internal/codexappgateway/server.go`
- Modify: `internal/codexappgateway/config.go`
- Modify: `internal/codexappgateway/server_test.go`
- Modify: `internal/codexappgateway/buildconfig_test.go`

- [ ] **Step 1: Extend ServeConfig with oplog fields**

In `internal/codexappgateway/config.go`, add to the `ServeConfig` struct:

```go
// OperationLog endpoint + auth. When both are empty, oplog is disabled
// (the Interceptor still parses frames but Submit is a no-op).
OperationLogURL    string // e.g. "http://agentserver.svc.cluster.local:8080/internal/operations"
OperationLogSecret string // AgentserverInternalSecret
OperationLogChan   int    // bounded channel capacity, default 1024
```

Update `parseServeArgs` (or equivalent) to read corresponding env vars:
`CXG_OPLOG_URL`, `CXG_OPLOG_SECRET`, `CXG_OPLOG_CHAN`. Apply env-fallback pattern.

Update `buildconfig_test.go` to cover the new env vars.

- [ ] **Step 2: Add `/notebook/ws` route**

In `server.go`, the existing `Routes()` registers `/` and `/codex-app/ws`. Add:

```go
r.Get("/notebook/ws", s.handleNotebookWS)
```

- [ ] **Step 3: Implement handleNotebookWS**

Add to `server.go`. The TUI path uses `wsbridge.RunProxy`; the SDK path uses `RunProxyWithInterceptor` with oplog wired in:

```go
func (s *Server) handleNotebookWS(w http.ResponseWriter, r *http.Request) {
	tok, ok := auth.ExtractBearer(r)
	if !ok {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}
	id, err := s.auth.Verify(r.Context(), tok)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	userWS, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Warn("notebook ws accept", "err", err)
		return
	}
	userWS.SetReadLimit(maxWSFrameBytes)
	defer userWS.Close(websocket.StatusNormalClosure, "client closing")

	ctx := r.Context()
	key := supervisor.Key{WorkspaceID: id.WorkspaceID}
	handle, err := s.sup.EnsureSubprocess(ctx, key, func(loopbackToken string) (supervisor.SpawnConfig, error) {
		return s.buildConfig(ctx, id.WorkspaceID, loopbackToken)
	})
	if err != nil {
		s.logger.Error("ensure subprocess", "err", err, "key", key)
		_ = userWS.Close(websocket.StatusInternalError, "subprocess unavailable")
		return
	}

	childWS, _, err := websocket.Dial(ctx, handle.WSURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.logger.Error("dial child", "err", err, "url", handle.WSURL)
		_ = userWS.Close(websocket.StatusInternalError, "subprocess dial failed")
		return
	}
	childWS.SetReadLimit(maxWSFrameBytes)
	defer childWS.Close(websocket.StatusNormalClosure, "gateway closing")

	// Per-connection Interceptor — workspaceID is pinned from the
	// verified bearer Identity, not from any client-supplied tag.
	// nil when oplog is disabled (CXG_OPLOG_URL empty); the proxy still
	// runs without logging.
	var perConn *oplog.Interceptor
	if s.oplogClient != nil {
		perConn = oplog.NewInterceptor(s.oplogClient, oplog.Config{
			Source: "sdk", WorkspaceID: id.WorkspaceID,
		})
	}

	intc := wsbridge.Interceptor{
		OnClientFrame: func(frame []byte) []byte {
			// 1) Intercept operations/list — handled by gateway, never
			//    forwarded to codex (which has no such method).
			if s.oplogList != nil {
				if resp, handled := oplog.TryHandleOperationsList(ctx, s.oplogList, id.WorkspaceID, frame); handled {
					// Write response straight back on userWS; tell the
					// pump to drop the original outgoing frame.
					_ = userWS.Write(ctx, websocket.MessageText, resp)
					return wsbridge.DropFrame
				}
			}
			// 2) Otherwise observe for oplog write side.
			if perConn != nil {
				perConn.OnClientFrame(frame)
			}
			return nil
		},
		OnServerFrame: func(frame []byte) []byte {
			if perConn != nil {
				perConn.OnServerFrame(frame)
			}
			return nil
		},
	}

	s.sup.Touch(key)
	if err := wsbridge.RunProxyWithInterceptor(ctx, userWS, childWS, intc, func() {
		s.sup.Touch(key)
	}); err != nil {
		s.logger.Info("notebook proxy ended", "err", err, "key", key)
	}
}
```

- [ ] **Step 4: Teach wsbridge to swallow frames (DropFrame sentinel)**

Edit `internal/wsbridge/wsbridge.go`. Add at package scope:

```go
import "bytes"

// DropFrame is returned by an Interceptor callback to indicate the frame
// should NOT be forwarded downstream. Distinct from returning nil
// (forward unchanged) and from returning a rewritten slice (forward
// replacement).
var DropFrame = []byte("__wsbridge_drop_frame__")
```

Update `pumpWithIntercept` (added in Step 3) to honor it:

```go
func pumpWithIntercept(
	ctx context.Context,
	src, dst *websocket.Conn,
	onFrameBytes func([]byte) []byte,
	onTick func(),
) error {
	for {
		mt, data, err := src.Read(ctx)
		if err != nil {
			closeErr := websocket.CloseStatus(err)
			if closeErr == websocket.StatusNormalClosure || closeErr == websocket.StatusGoingAway {
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		out := data
		if onFrameBytes != nil {
			if rewritten := onFrameBytes(data); rewritten != nil {
				if bytes.Equal(rewritten, DropFrame) {
					if onTick != nil {
						onTick()
					}
					continue // do not forward
				}
				out = rewritten
			}
		}
		if onTick != nil {
			onTick()
		}
		if err := dst.Write(ctx, mt, out); err != nil {
			return err
		}
	}
}
```

Add a test for the drop path. Append to `wsbridge_test.go`:

```go
func TestRunProxyWithInterceptor_DropFrameSwallows(t *testing.T) {
	a, b := newWSPair(t)
	defer a.Close(websocket.StatusNormalClosure, "")
	defer b.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go wsbridge.RunProxyWithInterceptor(ctx, a, b, wsbridge.Interceptor{
		OnClientFrame: func(frame []byte) []byte {
			if string(frame) == "drop-me" {
				return wsbridge.DropFrame
			}
			return nil
		},
	}, nil)

	if err := writeText(a, "drop-me"); err != nil {
		t.Fatal(err)
	}
	// b must not receive within a short window
	bctx, bcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer bcancel()
	_, _, err := b.Read(bctx)
	if err == nil {
		t.Fatal("b should not have received the dropped frame")
	}
}
```

- [ ] **Step 5: Boot wires oplog client + list-client on Server**

In `server.go` `New`/constructor (whatever exists), add fields and init:

```go
type Server struct {
    // ...existing
    oplogClient   *oplog.Client      // nil if disabled
    oplogList     *oplog.ListClient  // nil if disabled
}

func New(cfg ServeConfig, ...) *Server {
    s := &Server{...}
    if cfg.OperationLogURL != "" && cfg.OperationLogSecret != "" {
        s.oplogClient = oplog.NewClient(cfg.OperationLogURL, cfg.OperationLogSecret, cfg.OperationLogChan)
        s.oplogList   = oplog.NewListClient(cfg.OperationLogURL, cfg.OperationLogSecret)
    }
    return s
}
```

Add a `Close()` method (or extend existing) that calls `s.oplogClient.Close()`.

- [ ] **Step 6: Integration test via existing server_test.go pattern**

Update `internal/codexappgateway/server_test.go` to add a test that drives `/notebook/ws` with a stub agentserver oplog endpoint (httptest). Assert the stub received a POST after a single `mcpServer/tool/call` round-trip. Use the same testing harness already present (the existing test boots the gateway against a stub auth verifier — extend it).

- [ ] **Step 7: Run all gateway tests**

```bash
go test ./internal/codexappgateway/... -v
go test ./internal/wsbridge -v
```
Expected: all pass.

- [ ] **Step 8: Commit**

```bash
cd /root/agentserver
git add internal/codexappgateway/server.go \
        internal/codexappgateway/config.go \
        internal/codexappgateway/server_test.go \
        internal/codexappgateway/buildconfig_test.go \
        internal/wsbridge/wsbridge.go \
        internal/wsbridge/wsbridge_test.go
git commit -m "feat(gateway): /notebook/ws route + oplog interceptor wiring

new route path = source 'sdk'; per-connection Interceptor with workspaceID;
operations/list intercepted and served from agentserver without reaching
codex app-server. wsbridge.DropFrame lets the interceptor swallow frames."
```

---

## Task 9: Helm + values plumbing

**Files:**
- Modify: `deploy/helm/agentserver/values.yaml`
- Modify: `deploy/helm/agentserver/templates/codex-app-gateway.yaml`
- Modify: `deploy/helm/agentserver/templates/_helpers.tpl` if a helper for the internal secret already exists

- [ ] **Step 1: Add values block**

Append to `values.yaml`:

```yaml
operations:
  # When true, codex-app-gateway POSTs every mcpServer/tool/call to
  # agentserver's /internal/operations. Disable to revert to pre-Plan-2
  # behavior.
  enabled: true
  # Retention in days. 0 disables the cleanup loop.
  retentionDays: 90
  # Bounded channel capacity inside the gateway. Drops on overflow.
  channelCapacity: 1024
```

- [ ] **Step 2: Project values into gateway env**

Edit `templates/codex-app-gateway.yaml` Deployment env block:

```yaml
{{- if .Values.operations.enabled }}
- name: CXG_OPLOG_URL
  value: "http://{{ include "agentserver.fullname" . }}:8080/internal/operations"
- name: CXG_OPLOG_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ include "agentserver.fullname" . }}-internal
      key: internal-secret
- name: CXG_OPLOG_CHAN
  value: "{{ .Values.operations.channelCapacity }}"
{{- end }}
```

Use whatever helper / secret name the existing chart already uses for the agentserver internal secret. If unsure, grep:

```bash
grep -rn "internal-secret\|AGENTSERVER_INTERNAL_SECRET" /root/agentserver/deploy/helm/
```

- [ ] **Step 3: Pass retention via agentserver env**

Add to `templates/deployment.yaml` (the agentserver web deployment) env:

```yaml
- name: AGENTSERVER_OPERATIONS_RETENTION_DAYS
  value: "{{ .Values.operations.retentionDays }}"
```

Then in `main.go` (agentserver), read this env var and pass to `startRetentionLoop`. Default to 90 if unset.

- [ ] **Step 4: helm lint + template sanity**

```bash
cd /root/agentserver
helm lint deploy/helm/agentserver
helm template deploy/helm/agentserver | grep -A 5 CXG_OPLOG_URL | head -20
```

Expected: lint clean; rendered Deployment contains the new env vars.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/agentserver/values.yaml \
        deploy/helm/agentserver/templates/codex-app-gateway.yaml \
        deploy/helm/agentserver/templates/deployment.yaml \
        main.go
git commit -m "feat(helm): wire operation log config

values.operations.{enabled, retentionDays, channelCapacity}; gateway
gets CXG_OPLOG_{URL,SECRET,CHAN}; agentserver gets retention env var."
```

---

## Task 10: SDK e2e smoke against real-shape stub

**Files:**
- Modify: `notebook/stub_gateway_runner.py` (add a tiny operations/list handler with real data shape so SDK's `ctx.history()` returns non-empty)
- Modify: `notebook/README.md` — Cell 4 expectation now shows real records

- [ ] **Step 1: Update stub to return operations**

Edit `/root/agentserver/notebook/stub_gateway_runner.py`. Replace the empty `operations/list` handler with one that returns a few synthetic ops mirroring what Plan 2 will produce in real life:

```python
import uuid, datetime
def operations_list(p):
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    return {"operations": [
        {"id": str(uuid.uuid4()), "env_id": "alpha", "tool": "shell",
         "is_error": False, "started_at": now, "duration_ms": 7,
         "source": "sdk", "user_id": "smoke-user"},
        {"id": str(uuid.uuid4()), "env_id": "hpc",   "tool": "submit_task",
         "is_error": False, "started_at": now, "duration_ms": 320,
         "source": "sdk", "user_id": "smoke-user"},
    ]}
g.on("operations/list", operations_list)
```

- [ ] **Step 2: Bring up smoke + manual verify**

```bash
cd /root/agentserver
docker compose -f notebook/docker-compose.smoke.yml up --build -d
sleep 10
docker compose -f notebook/docker-compose.smoke.yml logs notebook | grep "running"
```

Manual:
1. Open <http://localhost:8888/lab>
2. Cell: `ops = await ctx.history(limit=5); ops`
3. Expect: 2 OperationRecord objects, env_id={alpha,hpc}

Teardown:
```bash
docker compose -f notebook/docker-compose.smoke.yml down -v
```

- [ ] **Step 3: Update README**

In `notebook/README.md` Cell 4 expectation, change from "returns [] until Plan 2 lands" to:

```
# Cell 4 — operations history (real records once Plan 2 is deployed)
await ctx.history(limit=5)
# stub returns 2 fake records; real backend returns rows from the
# operations table written by every previous SDK call.
```

- [ ] **Step 4: Commit**

```bash
cd /root/agentserver
git add notebook/stub_gateway_runner.py notebook/README.md
git commit -m "test(notebook): stub returns synthetic operations for smoke

Cell 4 in the walkthrough now returns 2 records, exercising ctx.history
end-to-end through the stub."
```

---

## Self-review checklist (for the implementer)

After all tasks done:

- [ ] All spec § 7 schema columns present in 023_operations.sql
- [ ] Gateway never blocks on logging (verified: `TestClient_Submit_BoundedDropsOnFull`)
- [ ] Source tag derived from ws path, not client-supplied (verified by code review of server.go diff)
- [ ] `operations/list` does NOT reach `codex app-server` (verified by code review: `TryHandleOperationsList` returns frame that's written directly back, and `DropFrame` is returned to swallow the original from going downstream)
- [ ] Payload truncation thresholds 64KiB args / 4KiB result enforced; sha256 + size recorded when truncated
- [ ] Retention loop exits on context cancel
- [ ] Helm renders cleanly + env vars correct
- [ ] Notebook smoke walkthrough still works after stub update

## After this plan

When Plan 2 is merged:
- `ctx.history(...)` returns real data
- Web UI `<OperationsPanel />` (Plan 3) only needs a frontend; backend `GET /internal/operations` is ready
- LLM-call logging (v1.5): env-mcp emits `Operation` records via the same `/internal/operations` POST endpoint — no new infra needed
- Per-workspace retention overrides: extend `OperationFilter` / add new column `workspace_settings.retention_days`
