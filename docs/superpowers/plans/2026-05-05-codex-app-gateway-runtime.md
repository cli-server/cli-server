# codex-app-gateway Runtime Implementation Plan (2b of 4)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Layer the runtime — JSON-RPC handlers, codex driver, event mapper,
session worker, recovery, revocation, and an end-to-end test — on top of the
codex-app-gateway foundations so a single codex turn round-trips end-to-end:
ws connect → `initialize` → `thread/start` → `turn/start` → live
`ServerNotification` stream → persisted events → terminal revocation push.

**Architecture:** Pure handlers translate `protocol.ClientRequest`s into
store calls plus a one-shot worker wake-up. The per-thread session worker
serializes turns: download workspace → write manifest → mint capability
token → spawn codex via `codex-agent-sdk-go` → pump `ThreadEvent`s through
a pure mapper into `ServerNotification`s → persist + push → POST revoke →
clean up. Recovery on startup resets stale `running` rows back to `queued`
and pings affected workers.

**Tech Stack:** Go 1.26; `github.com/agentserver/codex-agent-sdk-go`
(driver); `nhooyr.io/websocket` (transport, established in 2a);
`net/http` (egress to codex-exec-gateway); `database/sql` via 2a's
`store.Store`. No new external deps beyond what 2a introduces.

**Spec:** `/root/agentserver/docs/superpowers/specs/2026-05-05-codex-app-gateway-and-exec-gateway-design.md`
(read § Subsystem 2: Turn lifecycle, Manifest construction, Capability
token, Workspace persistence, Spawning codex; § Open risks 4 for
revocation semantics; § Phase 1 vs deferred for the 17-RPC surface).

**Dependency note:** Depends on Plan 2a (foundations) being in place;
type names from 2a are referenced here unchanged. Specifically this
plan assumes 2a has shipped:

- `internal/codexappgateway/protocol`: `ClientRequest`, `ServerNotification`,
  `Thread`, `Turn`, `Item`, `Usage`, `InitializeResponse`, `TurnStartResponse`
- `internal/codexappgateway/transport`: `JSONRPCConn` (Read / Write /
  WriteNotification helpers, ws envelope)
- `internal/codexappgateway/store`: `Store` with `CreateThread`,
  `GetThread`, `ListThreads`, `ListTurns`, `EnqueueTurn`, `PickNextPending`,
  `MarkTurnRunning`, `MarkTurnDone`, `MarkTurnFailed`, `MarkTurnCancelled`,
  `InsertEvent`, `ListEvents`, `ResetRunningToQueued`,
  `ListThreadsWithPending`. Row types `Thread`, `AgentTurn`, `TurnEvent`.
- `internal/storage/agentworkspace`: `CodexLayout` (per-turn tmp dir
  with `CodexHome` + `ProjectDir`), `Setup(ctx, workspaceID, threadID)`,
  `Teardown(ctx, layout)`. Layout exposes `TmpRoot` so the manifest
  writer can place `exec_servers.json` alongside the workspace dirs.
- `internal/codexappgateway/exectoken`: `Mint(MintInput) (string, error)`,
  `Verify(secret []byte, token string) (Claims, error)`,
  `MintInput{Secret []byte, TurnID, WorkspaceID string, ExeIDs []string, TTL time.Duration, Now time.Time}`,
  `Claims{TurnID, WorkspaceID string, ExeIDs []string, IssuedAt, ExpiresAt int64}`.
- `internal/codexappgateway/server.go` exposing a `*Server` with
  `Store`, `Workspace`, `WSToken func(ctx, wid) (string, error)`,
  `Logger`, `Config`, `WorkerRegistry` (tested in 2a as a no-op).
- `internal/codexappgateway/connregistry`: per-conn-id pubsub keyed by
  `thread_id` so the worker can push live events to whichever ws
  connection currently owns that thread.

If any name above drifts in 2a, fix the import here — no other file
needs to change because all integration points are isolated to one
import block per file.

**Working directory:** All tasks operate in `/root/agentserver`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/codexappgateway/handlers/initialize.go` | `initialize` → `InitializeResponse` with phase-1 capability set |
| `internal/codexappgateway/handlers/thread.go` | `thread/start`, `thread/resume`, `thread/read`, `thread/list`, `thread/turns/list` |
| `internal/codexappgateway/handlers/turn.go` | `turn/start` (enqueue + 200 immediate), `turn/interrupt` (cancel) |
| `internal/codexappgateway/handlers/dispatch.go` | Method-string → handler routing for `ClientRequest` |
| `internal/codexappgateway/runner/manifest.go` | `BuildManifest`, `WriteManifest`, exec-gateway HTTP probe |
| `internal/codexappgateway/runner/runner.go` | `Driver` interface + `SDKDriver` (real `codex-agent-sdk-go` wiring) |
| `internal/codexappgateway/runner/event_mapper.go` | Pure `MapEvent(ThreadEvent, turnID, threadID) []ServerNotification` |
| `internal/codexappgateway/session_worker.go` | Per-thread queue drain: workspace setup → manifest → mint → run → pump → teardown |
| `internal/codexappgateway/worker_registry.go` | `Notify(threadID)`, `Cancel(threadID, turnID)` (active turn cancel funcs) |
| `internal/codexappgateway/recovery.go` | Startup: `ResetRunningToQueued`, ping workers for threads with pending |
| `internal/codexappgateway/revoke.go` | Fan-out POST to every configured exec-gateway URL on terminal state |
| `internal/codexappgateway/e2e_test.go` | Full round-trip with fake driver + fake exec-gateway + in-mem store |
| `internal/codexappgateway/handlers/*_test.go` | Per-handler table-driven tests |
| `internal/codexappgateway/runner/*_test.go` | Per-source-file unit tests |

---

## Task 1: `initialize` handler

**Files:**
- Create: `internal/codexappgateway/handlers/initialize.go`
- Create: `internal/codexappgateway/handlers/initialize_test.go`

- [ ] **Step 1: Write the failing test**

`internal/codexappgateway/handlers/initialize_test.go`:
```go
package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
)

func TestInitialize_ReturnsPhase1Capabilities(t *testing.T) {
	h := NewInitialize()
	resp, err := h.Handle(context.Background(), nil, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	got, ok := resp.(protocol.InitializeResponse)
	if !ok {
		t.Fatalf("want InitializeResponse, got %T", resp)
	}
	if got.ProtocolVersion != "v2" {
		t.Errorf("protocol_version: got %q want v2", got.ProtocolVersion)
	}
	if !got.Capabilities.Threads || !got.Capabilities.Turns {
		t.Errorf("missing core caps: %+v", got.Capabilities)
	}
	if got.Capabilities.Approvals {
		t.Errorf("approvals must be false in phase 1")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/codexappgateway/handlers/ -run TestInitialize -v`
Expected: FAIL with `undefined: NewInitialize`.

- [ ] **Step 3: Write the handler**

`internal/codexappgateway/handlers/initialize.go`:
```go
package handlers

import (
	"context"
	"encoding/json"

	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
)

// Initialize handles the codex app-server v2 `initialize` request.
type Initialize struct{}

func NewInitialize() *Initialize { return &Initialize{} }

// ConnState is the per-connection mutable state passed to every handler.
// Defined in package handlers so all handler files share one type.
type ConnState struct {
	UserID      string
	WorkspaceID string
	ConnID      string
	Initialized bool
}

func (h *Initialize) Handle(_ context.Context, st *ConnState, _ json.RawMessage) (any, error) {
	if st != nil {
		st.Initialized = true
	}
	return protocol.InitializeResponse{
		ProtocolVersion: "v2",
		ServerInfo: protocol.ServerInfo{
			Name:    "codex-app-gateway",
			Version: "phase1",
		},
		Capabilities: protocol.Capabilities{
			Threads:   true,
			Turns:     true,
			Approvals: false,
		},
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/codexappgateway/handlers/ -run TestInitialize -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/handlers/initialize.go internal/codexappgateway/handlers/initialize_test.go
git commit -m "feat(codex-app-gateway): initialize handler returns phase-1 capabilities"
```

---

## Task 2: Thread handlers (`thread/start|resume|read|list|turns/list`)

**Files:**
- Create: `internal/codexappgateway/handlers/thread.go`
- Create: `internal/codexappgateway/handlers/thread_test.go`

The handlers are thin wrappers over `store.Store`. Tests use a fake store
embedded in the same `_test.go` to keep the seam visible.

- [ ] **Step 1: Write the fake store + failing test**

`internal/codexappgateway/handlers/thread_test.go`:
```go
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
	"github.com/agentserver/agentserver/internal/codexappgateway/store"
)

type fakeStore struct {
	threads     map[string]store.Thread
	turns       map[string][]store.AgentTurn  // by thread_id
	events      map[string][]store.TurnEvent  // by turn_id
	createErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		threads: map[string]store.Thread{},
		turns:   map[string][]store.AgentTurn{},
		events:  map[string][]store.TurnEvent{},
	}
}

func (f *fakeStore) CreateThread(_ context.Context, t store.Thread) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.threads[t.ID] = t
	return nil
}
func (f *fakeStore) GetThread(_ context.Context, id string) (store.Thread, error) {
	t, ok := f.threads[id]
	if !ok {
		return store.Thread{}, errors.New("not found")
	}
	return t, nil
}
func (f *fakeStore) ListThreads(_ context.Context, wid string, _, _ int) ([]store.Thread, error) {
	var out []store.Thread
	for _, t := range f.threads {
		if t.WorkspaceID == wid {
			out = append(out, t)
		}
	}
	return out, nil
}
func (f *fakeStore) ListTurns(_ context.Context, tid string, _, _ int) ([]store.AgentTurn, error) {
	return f.turns[tid], nil
}
func (f *fakeStore) ListEvents(_ context.Context, turnID string, _ int64) ([]store.TurnEvent, error) {
	return f.events[turnID], nil
}

func TestThreadStart_PersistsAndReturnsThread(t *testing.T) {
	fs := newFakeStore()
	h := NewThreadHandlers(fs, nil, fixedClock(time.Unix(1714867200, 0)))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	body := json.RawMessage(`{"title":"t1"}`)
	resp, err := h.Start(context.Background(), st, body)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	tr := resp.(protocol.ThreadStartResponse)
	if tr.Thread.ID == "" || tr.Thread.WorkspaceID != "ws_1" {
		t.Errorf("bad thread: %+v", tr.Thread)
	}
	if _, ok := fs.threads[tr.Thread.ID]; !ok {
		t.Errorf("not persisted")
	}
}

func TestThreadResume_RequiresOwnership(t *testing.T) {
	fs := newFakeStore()
	fs.threads["thr_x"] = store.Thread{ID: "thr_x", WorkspaceID: "ws_other", UserID: "u_other"}
	h := NewThreadHandlers(fs, nil, fixedClock(time.Now()))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	body := json.RawMessage(`{"threadId":"thr_x"}`)
	_, err := h.Resume(context.Background(), st, body)
	if err == nil {
		t.Fatalf("want forbidden error, got nil")
	}
}

func TestThreadRead_ReturnsPersistedEvents(t *testing.T) {
	fs := newFakeStore()
	fs.threads["thr_1"] = store.Thread{ID: "thr_1", WorkspaceID: "ws_1", UserID: "u_1"}
	fs.turns["thr_1"] = []store.AgentTurn{{ID: "trn_1", ThreadID: "thr_1"}}
	fs.events["trn_1"] = []store.TurnEvent{
		{TurnID: "trn_1", SeqNum: 1, Payload: json.RawMessage(`{"method":"turn/started"}`)},
	}
	h := NewThreadHandlers(fs, nil, fixedClock(time.Now()))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	resp, err := h.Read(context.Background(), st, json.RawMessage(`{"threadId":"thr_1"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	tr := resp.(protocol.ThreadReadResponse)
	if len(tr.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(tr.Events))
	}
}

func TestThreadList_FiltersByWorkspace(t *testing.T) {
	fs := newFakeStore()
	fs.threads["a"] = store.Thread{ID: "a", WorkspaceID: "ws_1"}
	fs.threads["b"] = store.Thread{ID: "b", WorkspaceID: "ws_other"}
	h := NewThreadHandlers(fs, nil, fixedClock(time.Now()))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	resp, _ := h.List(context.Background(), st, json.RawMessage(`{}`))
	tr := resp.(protocol.ThreadListResponse)
	if len(tr.Threads) != 1 || tr.Threads[0].ID != "a" {
		t.Errorf("filter broken: %+v", tr.Threads)
	}
}

func TestThreadTurnsList_OK(t *testing.T) {
	fs := newFakeStore()
	fs.threads["thr_1"] = store.Thread{ID: "thr_1", WorkspaceID: "ws_1"}
	fs.turns["thr_1"] = []store.AgentTurn{{ID: "trn_1"}, {ID: "trn_2"}}
	h := NewThreadHandlers(fs, nil, fixedClock(time.Now()))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	resp, _ := h.TurnsList(context.Background(), st, json.RawMessage(`{"threadId":"thr_1"}`))
	tr := resp.(protocol.TurnListResponse)
	if len(tr.Turns) != 2 {
		t.Errorf("want 2 turns, got %d", len(tr.Turns))
	}
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/codexappgateway/handlers/ -run TestThread -v`
Expected: FAIL — `NewThreadHandlers`, `protocol.ThreadStartResponse`,
etc., undefined. (If `protocol.*` types from 2a are missing, stop and
finish 2a first.)

- [ ] **Step 3: Implement the handlers**

`internal/codexappgateway/handlers/thread.go`:
```go
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
	"github.com/agentserver/agentserver/internal/codexappgateway/store"
)

// ThreadStore is the subset of store.Store the thread handlers need. Defined
// as an interface so tests can substitute an in-memory fake.
type ThreadStore interface {
	CreateThread(ctx context.Context, t store.Thread) error
	GetThread(ctx context.Context, id string) (store.Thread, error)
	ListThreads(ctx context.Context, workspaceID string, limit, offset int) ([]store.Thread, error)
	ListTurns(ctx context.Context, threadID string, limit, offset int) ([]store.AgentTurn, error)
	ListEvents(ctx context.Context, turnID string, sinceSeq int64) ([]store.TurnEvent, error)
}

// WorkspaceFetcher is the subset of agentworkspace used by `thread/resume`.
type WorkspaceFetcher interface {
	Prefetch(ctx context.Context, workspaceID, threadID string) error
}

type ThreadHandlers struct {
	store ThreadStore
	ws    WorkspaceFetcher
	now   func() time.Time
}

func NewThreadHandlers(s ThreadStore, ws WorkspaceFetcher, now func() time.Time) *ThreadHandlers {
	if now == nil {
		now = time.Now
	}
	return &ThreadHandlers{store: s, ws: ws, now: now}
}

type threadStartReq struct {
	Title    string         `json:"title,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (h *ThreadHandlers) Start(ctx context.Context, st *ConnState, raw json.RawMessage) (any, error) {
	if err := requireInit(st); err != nil {
		return nil, err
	}
	var req threadStartReq
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode thread/start: %w", err)
		}
	}
	id := newID("thr_")
	now := h.now().UTC()
	metaJSON, _ := json.Marshal(req.Metadata)
	th := store.Thread{
		ID:          id,
		WorkspaceID: st.WorkspaceID,
		UserID:      st.UserID,
		Title:       req.Title,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
		Metadata:    metaJSON,
	}
	if err := h.store.CreateThread(ctx, th); err != nil {
		return nil, fmt.Errorf("create thread: %w", err)
	}
	return protocol.ThreadStartResponse{Thread: toProtocolThread(th)}, nil
}

type threadResumeReq struct {
	ThreadID string `json:"threadId"`
}

func (h *ThreadHandlers) Resume(ctx context.Context, st *ConnState, raw json.RawMessage) (any, error) {
	if err := requireInit(st); err != nil {
		return nil, err
	}
	var req threadResumeReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode thread/resume: %w", err)
	}
	th, err := h.store.GetThread(ctx, req.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}
	if th.UserID != st.UserID || th.WorkspaceID != st.WorkspaceID {
		return nil, errForbidden("thread not owned by caller")
	}
	if h.ws != nil {
		if err := h.ws.Prefetch(ctx, th.WorkspaceID, th.ID); err != nil {
			// Prefetch is best-effort: the worker re-downloads on turn/start.
			// Surface as warning via the return value's diagnostic field, not
			// a hard error.
			return protocol.ThreadResumeResponse{
				Thread:     toProtocolThread(th),
				Diagnostic: "workspace prefetch failed: " + err.Error(),
			}, nil
		}
	}
	return protocol.ThreadResumeResponse{Thread: toProtocolThread(th)}, nil
}

type threadReadReq struct {
	ThreadID string `json:"threadId"`
}

func (h *ThreadHandlers) Read(ctx context.Context, st *ConnState, raw json.RawMessage) (any, error) {
	if err := requireInit(st); err != nil {
		return nil, err
	}
	var req threadReadReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode thread/read: %w", err)
	}
	th, err := h.store.GetThread(ctx, req.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}
	if th.UserID != st.UserID || th.WorkspaceID != st.WorkspaceID {
		return nil, errForbidden("thread not owned by caller")
	}
	turns, err := h.store.ListTurns(ctx, req.ThreadID, 1000, 0)
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	var events []protocol.PersistedEvent
	for _, tn := range turns {
		evs, err := h.store.ListEvents(ctx, tn.ID, 0)
		if err != nil {
			return nil, fmt.Errorf("list events for %s: %w", tn.ID, err)
		}
		for _, e := range evs {
			events = append(events, protocol.PersistedEvent{
				TurnID:  tn.ID,
				SeqNum:  e.SeqNum,
				Payload: e.Payload,
			})
		}
	}
	return protocol.ThreadReadResponse{
		Thread: toProtocolThread(th),
		Turns:  toProtocolTurns(turns),
		Events: events,
	}, nil
}

type threadListReq struct {
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

func (h *ThreadHandlers) List(ctx context.Context, st *ConnState, raw json.RawMessage) (any, error) {
	if err := requireInit(st); err != nil {
		return nil, err
	}
	var req threadListReq
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if req.Limit <= 0 || req.Limit > 200 {
		req.Limit = 50
	}
	ths, err := h.store.ListThreads(ctx, st.WorkspaceID, req.Limit, req.Offset)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	out := make([]protocol.Thread, len(ths))
	for i, t := range ths {
		out[i] = toProtocolThread(t)
	}
	return protocol.ThreadListResponse{Threads: out}, nil
}

type turnsListReq struct {
	ThreadID string `json:"threadId"`
	Limit    int    `json:"limit,omitempty"`
	Offset   int    `json:"offset,omitempty"`
}

func (h *ThreadHandlers) TurnsList(ctx context.Context, st *ConnState, raw json.RawMessage) (any, error) {
	if err := requireInit(st); err != nil {
		return nil, err
	}
	var req turnsListReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode thread/turns/list: %w", err)
	}
	th, err := h.store.GetThread(ctx, req.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}
	if th.UserID != st.UserID || th.WorkspaceID != st.WorkspaceID {
		return nil, errForbidden("thread not owned by caller")
	}
	if req.Limit <= 0 || req.Limit > 200 {
		req.Limit = 50
	}
	turns, err := h.store.ListTurns(ctx, req.ThreadID, req.Limit, req.Offset)
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	return protocol.TurnListResponse{Turns: toProtocolTurns(turns)}, nil
}

func toProtocolThread(t store.Thread) protocol.Thread {
	return protocol.Thread{
		ID:          t.ID,
		WorkspaceID: t.WorkspaceID,
		Title:       t.Title,
		Status:      t.Status,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

func toProtocolTurns(in []store.AgentTurn) []protocol.Turn {
	out := make([]protocol.Turn, len(in))
	for i, t := range in {
		out[i] = protocol.Turn{
			ID:          t.ID,
			ThreadID:    t.ThreadID,
			Status:      t.Status,
			EnqueuedAt:  t.EnqueuedAt,
			StartedAt:   t.StartedAt,
			FinishedAt:  t.FinishedAt,
		}
	}
	return out
}

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func requireInit(st *ConnState) error {
	if st == nil || !st.Initialized {
		return errors.New("connection not initialized; send `initialized` first")
	}
	return nil
}

func errForbidden(msg string) error { return forbiddenError{msg: msg} }

type forbiddenError struct{ msg string }

func (e forbiddenError) Error() string { return "forbidden: " + e.msg }
func (forbiddenError) JSONRPCCode() int { return -32003 }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/codexappgateway/handlers/ -run TestThread -v`
Expected: PASS for all 5 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/handlers/thread.go internal/codexappgateway/handlers/thread_test.go
git commit -m "feat(codex-app-gateway): thread handlers (start/resume/read/list/turns)"
```

---

## Task 3: `turn/start` handler (enqueue + immediate response)

**Files:**
- Create: `internal/codexappgateway/handlers/turn.go`
- Create: `internal/codexappgateway/handlers/turn_test.go`

`turn/start` is intentionally lightweight: validate, INSERT
`codex_turns` row, notify the per-thread worker, return
`TurnStartResponse{turn:{id, status:"inProgress"}}` synchronously.
The session worker (Task 8) does the actual work asynchronously.

- [ ] **Step 1: Write the failing test**

`internal/codexappgateway/handlers/turn_test.go`:
```go
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
	"github.com/agentserver/agentserver/internal/codexappgateway/store"
)

type fakeTurnStore struct {
	*fakeStore
	enqueued []store.AgentTurn
	enqErr   error
}

func (f *fakeTurnStore) EnqueueTurn(_ context.Context, t store.AgentTurn) error {
	if f.enqErr != nil {
		return f.enqErr
	}
	f.enqueued = append(f.enqueued, t)
	return nil
}

type fakeNotifier struct {
	mu       sync.Mutex
	notified []string
	cancels  map[string]map[string]bool // threadID → turnID → present
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{cancels: map[string]map[string]bool{}}
}
func (f *fakeNotifier) Notify(threadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, threadID)
}
func (f *fakeNotifier) Cancel(threadID, turnID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.cancels[threadID]
	if !ok {
		return false
	}
	return m[turnID]
}

func TestTurnStart_EnqueuesAndReturnsInProgress(t *testing.T) {
	fs := &fakeTurnStore{fakeStore: newFakeStore()}
	fs.threads["thr_1"] = store.Thread{ID: "thr_1", WorkspaceID: "ws_1", UserID: "u_1"}
	notif := newFakeNotifier()
	h := NewTurnHandlers(fs, notif, fixedClock(timeFromUnix(1714867200)))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", ConnID: "c_1", Initialized: true}
	body := json.RawMessage(`{"threadId":"thr_1","input":[{"type":"text","text":"hi"}]}`)
	resp, err := h.Start(context.Background(), st, body)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	tr := resp.(protocol.TurnStartResponse)
	if tr.Turn.Status != "inProgress" {
		t.Errorf("status: got %q want inProgress", tr.Turn.Status)
	}
	if len(fs.enqueued) != 1 || fs.enqueued[0].ThreadID != "thr_1" {
		t.Errorf("enqueue: %+v", fs.enqueued)
	}
	if len(notif.notified) != 1 || notif.notified[0] != "thr_1" {
		t.Errorf("notify: %+v", notif.notified)
	}
}

func TestTurnStart_EnqueueErrorBubbles(t *testing.T) {
	fs := &fakeTurnStore{fakeStore: newFakeStore(), enqErr: errors.New("db down")}
	fs.threads["thr_1"] = store.Thread{ID: "thr_1", WorkspaceID: "ws_1", UserID: "u_1"}
	notif := newFakeNotifier()
	h := NewTurnHandlers(fs, notif, fixedClock(timeFromUnix(0)))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	_, err := h.Start(context.Background(), st,
		json.RawMessage(`{"threadId":"thr_1","input":[{"type":"text","text":"hi"}]}`))
	if err == nil {
		t.Fatalf("want error")
	}
}

func TestTurnStart_RejectsForeignThread(t *testing.T) {
	fs := &fakeTurnStore{fakeStore: newFakeStore()}
	fs.threads["thr_x"] = store.Thread{ID: "thr_x", WorkspaceID: "ws_other", UserID: "u_other"}
	h := NewTurnHandlers(fs, newFakeNotifier(), fixedClock(timeFromUnix(0)))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	_, err := h.Start(context.Background(), st,
		json.RawMessage(`{"threadId":"thr_x","input":[{"type":"text","text":"hi"}]}`))
	if err == nil {
		t.Fatalf("want forbidden")
	}
}
```

Add helper at end of `thread_test.go` (or here):
```go
func timeFromUnix(s int64) time.Time { return time.Unix(s, 0).UTC() }
```

- [ ] **Step 2: Run test, verify FAIL**

Run: `go test ./internal/codexappgateway/handlers/ -run TestTurnStart -v`
Expected: FAIL — `NewTurnHandlers` undefined.

- [ ] **Step 3: Implement the handler**

`internal/codexappgateway/handlers/turn.go`:
```go
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
	"github.com/agentserver/agentserver/internal/codexappgateway/store"
)

// TurnStore is the subset of store.Store the turn handlers need.
type TurnStore interface {
	GetThread(ctx context.Context, id string) (store.Thread, error)
	EnqueueTurn(ctx context.Context, t store.AgentTurn) error
}

// WorkerNotifier is the seam to the per-thread session worker registry.
type WorkerNotifier interface {
	Notify(threadID string)
	Cancel(threadID, turnID string) bool
}

type TurnHandlers struct {
	store TurnStore
	notif WorkerNotifier
	now   func() time.Time
}

func NewTurnHandlers(s TurnStore, n WorkerNotifier, now func() time.Time) *TurnHandlers {
	if now == nil {
		now = time.Now
	}
	return &TurnHandlers{store: s, notif: n, now: now}
}

type turnStartReq struct {
	ThreadID     string          `json:"threadId"`
	Input        json.RawMessage `json:"input"`
	Cwd          string          `json:"cwd,omitempty"`
	Model        string          `json:"model,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Environments []string        `json:"environments,omitempty"`
}

func (h *TurnHandlers) Start(ctx context.Context, st *ConnState, raw json.RawMessage) (any, error) {
	if err := requireInit(st); err != nil {
		return nil, err
	}
	var req turnStartReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode turn/start: %w", err)
	}
	if req.ThreadID == "" {
		return nil, errors.New("threadId required")
	}
	if len(req.Input) == 0 {
		return nil, errors.New("input required")
	}
	th, err := h.store.GetThread(ctx, req.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}
	if th.UserID != st.UserID || th.WorkspaceID != st.WorkspaceID {
		return nil, errForbidden("thread not owned by caller")
	}
	turnID := newID("trn_")
	// Internal blob persisted to codex_turns.turn_options (JSONB) — not on
	// the wire to clients. Keep keys snake_case to match the SQL column
	// convention; the worker reads them back into `opts` below.
	options, _ := json.Marshal(map[string]any{
		"cwd":           req.Cwd,
		"model":         req.Model,
		"output_schema": json.RawMessage(req.OutputSchema),
		"environments":  req.Environments,
		"conn_id":       st.ConnID,
	})
	turn := store.AgentTurn{
		ID:          turnID,
		ThreadID:    req.ThreadID,
		WorkspaceID: th.WorkspaceID,
		UserInput:   req.Input,
		TurnOptions: options,
		Status:      "pending",
		EnqueuedAt:  h.now().UTC(),
	}
	if err := h.store.EnqueueTurn(ctx, turn); err != nil {
		return nil, fmt.Errorf("enqueue turn: %w", err)
	}
	h.notif.Notify(req.ThreadID)
	return protocol.TurnStartResponse{
		Turn: protocol.Turn{
			ID:         turnID,
			ThreadID:   req.ThreadID,
			Status:     "inProgress",
			EnqueuedAt: turn.EnqueuedAt,
		},
	}, nil
}

type turnInterruptReq struct {
	TurnID   string `json:"turnId"`
	ThreadID string `json:"threadId"`
}

func (h *TurnHandlers) Interrupt(ctx context.Context, st *ConnState, raw json.RawMessage) (any, error) {
	if err := requireInit(st); err != nil {
		return nil, err
	}
	var req turnInterruptReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode turn/interrupt: %w", err)
	}
	th, err := h.store.GetThread(ctx, req.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}
	if th.UserID != st.UserID || th.WorkspaceID != st.WorkspaceID {
		return nil, errForbidden("thread not owned by caller")
	}
	cancelled := h.notif.Cancel(req.ThreadID, req.TurnID)
	return protocol.TurnInterruptResponse{Cancelled: cancelled}, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/codexappgateway/handlers/ -run TestTurnStart -v`
Expected: PASS for all three.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/handlers/turn.go internal/codexappgateway/handlers/turn_test.go
git commit -m "feat(codex-app-gateway): turn/start enqueues + responds in_progress"
```

---

## Task 4: `turn/interrupt` handler

The function body was added in Task 3; this task adds the dedicated test
to lock semantics: cancel only fires when the turn is currently running
in this pod, otherwise `cancelled:false` (the worker will still see the
DB row and short-circuit on its own when it picks the turn up).

**Files:**
- Modify: `internal/codexappgateway/handlers/turn_test.go`

- [ ] **Step 1: Add the failing test**

Append to `turn_test.go`:
```go
func TestTurnInterrupt_HitsActiveTurn(t *testing.T) {
	fs := &fakeTurnStore{fakeStore: newFakeStore()}
	fs.threads["thr_1"] = store.Thread{ID: "thr_1", WorkspaceID: "ws_1", UserID: "u_1"}
	notif := newFakeNotifier()
	notif.cancels["thr_1"] = map[string]bool{"trn_a": true}
	h := NewTurnHandlers(fs, notif, fixedClock(timeFromUnix(0)))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	resp, err := h.Interrupt(context.Background(), st,
		json.RawMessage(`{"turnId":"trn_a","threadId":"thr_1"}`))
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if !resp.(protocol.TurnInterruptResponse).Cancelled {
		t.Errorf("want cancelled:true")
	}
}

func TestTurnInterrupt_MissesUnknownTurn(t *testing.T) {
	fs := &fakeTurnStore{fakeStore: newFakeStore()}
	fs.threads["thr_1"] = store.Thread{ID: "thr_1", WorkspaceID: "ws_1", UserID: "u_1"}
	h := NewTurnHandlers(fs, newFakeNotifier(), fixedClock(timeFromUnix(0)))
	st := &ConnState{UserID: "u_1", WorkspaceID: "ws_1", Initialized: true}
	resp, _ := h.Interrupt(context.Background(), st,
		json.RawMessage(`{"turnId":"trn_x","threadId":"thr_1"}`))
	if resp.(protocol.TurnInterruptResponse).Cancelled {
		t.Errorf("want cancelled:false")
	}
}
```

- [ ] **Step 2: Run tests**

Run: `go test ./internal/codexappgateway/handlers/ -run TestTurnInterrupt -v`
Expected: PASS (handler already implemented in Task 3; this just locks
behavior).

- [ ] **Step 3: Commit**

```bash
git add internal/codexappgateway/handlers/turn_test.go
git commit -m "test(codex-app-gateway): turn/interrupt cancel-vs-miss semantics"
```

---

## Task 5: Manifest writer + exec-gateway probe

**Files:**
- Create: `internal/codexappgateway/runner/manifest.go`
- Create: `internal/codexappgateway/runner/manifest_test.go`

The manifest writer (a) probes exec-gateway over HTTP for live executors
in this workspace, (b) builds the JSON spec described in spec § Manifest
construction, (c) writes it to `/tmp/codex-app-gateway/<turn_id>/exec_servers.json`
with mode 0600. The minted `CODEX_EXEC_GATEWAY_TOKEN`'s `exe_ids` payload
is the manifest's `id` list (so writing and minting are paired in
`BuildAndWrite`).

- [ ] **Step 1: Write the failing test**

`internal/codexappgateway/runner/manifest_test.go`:
```go
package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/exectoken"
)

func TestBuildAndWriteManifest_EmitsExpectedSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/exec-gateway/connected" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("workspace_id") != "ws_1" {
			t.Errorf("workspace_id query: got %q", r.URL.Query().Get("workspace_id"))
		}
		_ = json.NewEncoder(w).Encode([]ConnectedExecutor{
			{ExeID: "exe_alpha", Description: "Daisy mac, /home/daisy", DefaultCwd: "/home/daisy", IsDefault: true},
			{ExeID: "exe_beta", Description: "EC2 us-east-1", DefaultCwd: "/var/proj"},
		})
	}))
	defer srv.Close()

	mw := NewManifestWriter(ManifestConfig{
		ExecGatewayHTTPURL:   srv.URL,
		ExecGatewayWSURL:     "ws://codex-exec-gateway:6060",
		InternalSharedSecret: "internal-shared",
		CapTokenHMACSecret:   []byte("hmac-secret"),
		TmpRoot:              t.TempDir(),
		HTTP:                 srv.Client(),
		Now:                  func() time.Time { return time.Unix(1714867200, 0).UTC() },
	})

	res, err := mw.BuildAndWrite(context.Background(), BuildInput{
		TurnID:      "trn_xyz",
		WorkspaceID: "ws_1",
	})
	if err != nil {
		t.Fatalf("BuildAndWrite: %v", err)
	}
	if filepath.Base(res.ManifestPath) != "exec_servers.json" {
		t.Errorf("path: %s", res.ManifestPath)
	}
	info, err := os.Stat(res.ManifestPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v", info.Mode())
	}
	body, _ := os.ReadFile(res.ManifestPath)
	var spec ManifestSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if spec.DefaultEnvironmentID != "exe_alpha" {
		t.Errorf("default: %s", spec.DefaultEnvironmentID)
	}
	if len(spec.Environments) != 2 {
		t.Fatalf("envs: %d", len(spec.Environments))
	}
	if spec.Environments[0].URL != "ws://codex-exec-gateway:6060/bridge/exe_alpha" {
		t.Errorf("url: %s", spec.Environments[0].URL)
	}
	if spec.Environments[0].AuthTokenEnv != "CODEX_EXEC_GATEWAY_TOKEN" {
		t.Errorf("auth_token_env: %s", spec.Environments[0].AuthTokenEnv)
	}
	// Token payload exe_ids must equal manifest ids (both order-preserving).
	claims, err := exectoken.Verify([]byte("hmac-secret"), res.CapToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(claims.ExeIDs) != 2 || claims.ExeIDs[0] != "exe_alpha" || claims.ExeIDs[1] != "exe_beta" {
		t.Errorf("token exe_ids: %v", claims.ExeIDs)
	}
	if claims.TurnID != "trn_xyz" {
		t.Errorf("token turn_id: %s", claims.TurnID)
	}
}

func TestBuildAndWriteManifest_NoLiveExecutors_ReturnsEmptyManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ConnectedExecutor{})
	}))
	defer srv.Close()
	mw := NewManifestWriter(ManifestConfig{
		ExecGatewayHTTPURL: srv.URL, ExecGatewayWSURL: "ws://x",
		InternalSharedSecret: "s", CapTokenHMACSecret: []byte("k"),
		TmpRoot: t.TempDir(), HTTP: srv.Client(),
		Now: time.Now,
	})
	res, err := mw.BuildAndWrite(context.Background(), BuildInput{TurnID: "trn_e", WorkspaceID: "ws_1"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	body, _ := os.ReadFile(res.ManifestPath)
	var spec ManifestSpec
	_ = json.Unmarshal(body, &spec)
	if len(spec.Environments) != 0 {
		t.Errorf("want empty envs, got %d", len(spec.Environments))
	}
	if spec.DefaultEnvironmentID != "" {
		t.Errorf("want empty default, got %s", spec.DefaultEnvironmentID)
	}
}
```

- [ ] **Step 2: Run test, verify FAIL**

Run: `go test ./internal/codexappgateway/runner/ -run TestBuildAndWrite -v`
Expected: FAIL — package or `NewManifestWriter` undefined.

- [ ] **Step 3: Implement the manifest writer**

`internal/codexappgateway/runner/manifest.go`:
```go
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/exectoken"
)

// ConnectedExecutor mirrors the JSON shape of one element of the
// `GET /api/exec-gateway/connected` response (spec § Internal API).
type ConnectedExecutor struct {
	ExeID       string    `json:"exe_id"`
	Description string    `json:"description"`
	DefaultCwd  string    `json:"default_cwd"`
	IsDefault   bool      `json:"is_default"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// ManifestSpec is the JSON written to `exec_servers.json`. Field tags
// follow spec § P1 ManifestEnvironmentProvider exactly.
type ManifestSpec struct {
	DefaultEnvironmentID string                `json:"default_environment_id,omitempty"`
	Environments         []ManifestEnvironment `json:"environments"`
}

type ManifestEnvironment struct {
	ID           string `json:"id"`
	URL          string `json:"url"`
	AuthTokenEnv string `json:"auth_token_env"`
	Description  string `json:"description"`
}

type ManifestConfig struct {
	ExecGatewayHTTPURL   string        // e.g. "http://codex-exec-gateway:6060"
	ExecGatewayWSURL     string        // e.g. "ws://codex-exec-gateway:6060"
	InternalSharedSecret string        // shared bearer for /api/exec-gateway/* (CXG_INTERNAL_SHARED_SECRET)
	CapTokenHMACSecret   []byte        // CODEX_EXEC_GATEWAY_TOKEN signing key (matches exectoken.MintInput.Secret)
	TmpRoot              string        // default "/tmp/codex-app-gateway"
	HTTP                 *http.Client
	Now                  func() time.Time
	TokenTTL             time.Duration // default 1h
}

type BuildInput struct {
	TurnID      string
	WorkspaceID string
	// PreferredDefaultID, if non-empty, overrides the workspace default.
	PreferredDefaultID string
}

type BuildResult struct {
	ManifestPath string   // export as CODEX_EXEC_SERVERS_JSON
	CapToken     string   // export as CODEX_EXEC_GATEWAY_TOKEN
	ExeIDs       []string // ids embedded in token + manifest
}

type ManifestWriter struct{ cfg ManifestConfig }

func NewManifestWriter(cfg ManifestConfig) *ManifestWriter {
	if cfg.TmpRoot == "" {
		cfg.TmpRoot = "/tmp/codex-app-gateway"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = time.Hour
	}
	return &ManifestWriter{cfg: cfg}
}

// BuildAndWrite probes exec-gateway, builds the manifest spec, mints the
// matching capability token, and writes the manifest to a per-turn tmp
// dir with mode 0600.
func (m *ManifestWriter) BuildAndWrite(ctx context.Context, in BuildInput) (BuildResult, error) {
	executors, err := m.probeConnected(ctx, in.WorkspaceID)
	if err != nil {
		return BuildResult{}, fmt.Errorf("probe exec-gateway: %w", err)
	}
	envs := make([]ManifestEnvironment, 0, len(executors))
	exeIDs := make([]string, 0, len(executors))
	defaultID := in.PreferredDefaultID
	for _, e := range executors {
		envs = append(envs, ManifestEnvironment{
			ID:           e.ExeID,
			URL:          fmt.Sprintf("%s/bridge/%s", m.cfg.ExecGatewayWSURL, e.ExeID),
			AuthTokenEnv: "CODEX_EXEC_GATEWAY_TOKEN",
			Description:  formatDescription(e),
		})
		exeIDs = append(exeIDs, e.ExeID)
		if defaultID == "" && e.IsDefault {
			defaultID = e.ExeID
		}
	}
	if defaultID == "" && len(envs) > 0 {
		defaultID = envs[0].ID
	}
	spec := ManifestSpec{DefaultEnvironmentID: defaultID, Environments: envs}

	dir := filepath.Join(m.cfg.TmpRoot, in.TurnID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BuildResult{}, fmt.Errorf("mkdir manifest tmp: %w", err)
	}
	path := filepath.Join(dir, "exec_servers.json")
	body, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return BuildResult{}, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return BuildResult{}, fmt.Errorf("write manifest: %w", err)
	}

	now := m.cfg.Now().UTC()
	tok, err := exectoken.Mint(exectoken.MintInput{
		Secret:      m.cfg.CapTokenHMACSecret,
		TurnID:      in.TurnID,
		WorkspaceID: in.WorkspaceID,
		ExeIDs:      exeIDs,
		TTL:         m.cfg.TokenTTL,
		Now:         now,
	})
	if err != nil {
		return BuildResult{}, fmt.Errorf("mint cap token: %w", err)
	}
	return BuildResult{ManifestPath: path, CapToken: tok, ExeIDs: exeIDs}, nil
}

func (m *ManifestWriter) probeConnected(ctx context.Context, workspaceID string) ([]ConnectedExecutor, error) {
	url := fmt.Sprintf("%s/api/exec-gateway/connected?workspace_id=%s",
		m.cfg.ExecGatewayHTTPURL, workspaceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.InternalSharedSecret)
	resp, err := m.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exec-gateway %s: status %d", url, resp.StatusCode)
	}
	var out []ConnectedExecutor
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode connected: %w", err)
	}
	return out, nil
}

func formatDescription(e ConnectedExecutor) string {
	if e.Description == "" && e.DefaultCwd == "" {
		return e.ExeID
	}
	if e.DefaultCwd == "" {
		return e.Description
	}
	if e.Description == "" {
		return e.DefaultCwd
	}
	return e.Description + " (" + e.DefaultCwd + ")"
}

// CleanupManifest removes the per-turn tmp dir created by BuildAndWrite.
// Safe to call multiple times.
func CleanupManifest(turnID, tmpRoot string) error {
	if tmpRoot == "" {
		tmpRoot = "/tmp/codex-app-gateway"
	}
	return os.RemoveAll(filepath.Join(tmpRoot, turnID))
}
```

- [ ] **Step 4: Run test**

Run: `go test ./internal/codexappgateway/runner/ -run TestBuildAndWrite -v`
Expected: PASS for both tests.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/runner/manifest.go internal/codexappgateway/runner/manifest_test.go
git commit -m "feat(codex-app-gateway): manifest writer + exec-gateway probe + token mint"
```

---

## Task 6: Driver wiring around `codex-agent-sdk-go`

**Files:**
- Create: `internal/codexappgateway/runner/runner.go`
- Create: `internal/codexappgateway/runner/runner_test.go`

A `Driver` interface abstracts the SDK so tests can inject a fake.
The real implementation `SDKDriver` constructs `codex.New(...)`,
calls `ResumeThread` or `StartThread` based on whether the gateway
already minted a `thread_id`, and returns a stream of `codex.ThreadEvent`
plus a `Wait()` callable. Phase-1: `ApprovalPolicy=never` always.

- [ ] **Step 1: Write the failing test**

`internal/codexappgateway/runner/runner_test.go`:
```go
package runner

import (
	"context"
	"testing"

	codex "github.com/agentserver/codex-agent-sdk-go"
)

func TestSDKDriver_BuildOptions_AppliesPhase1Defaults(t *testing.T) {
	d := NewSDKDriver(SDKConfig{
		LLMProxyURL: "https://llmproxy.example",
	})
	threadOpts := d.threadOptions(RunInput{
		Model:           "gpt-5",
		ProjectDir:      "/tmp/proj",
		ApprovalDefault: codex.ApprovalNever,
	})
	if threadOpts.ApprovalPolicy != codex.ApprovalNever {
		t.Errorf("approval: got %q want %q", threadOpts.ApprovalPolicy, codex.ApprovalNever)
	}
	if threadOpts.SandboxMode != codex.SandboxWorkspaceWrite {
		t.Errorf("sandbox: got %q", threadOpts.SandboxMode)
	}
	if threadOpts.WorkingDirectory != "/tmp/proj" {
		t.Errorf("cwd: got %q", threadOpts.WorkingDirectory)
	}
	if !threadOpts.SkipGitRepoCheck {
		t.Errorf("skip-git: false")
	}
	if threadOpts.Model != "gpt-5" {
		t.Errorf("model: got %q", threadOpts.Model)
	}
}

func TestSDKDriver_ComposeEnv_InjectsAllFour(t *testing.T) {
	d := NewSDKDriver(SDKConfig{LLMProxyURL: "x"})
	env := d.composeCodexEnv(RunInput{
		CodexHome:    "/tmp/x/codex-home",
		ManifestPath: "/tmp/x/exec_servers.json",
		CapToken:     "tokenX",
		WorkspaceTok: "wsTok",
	})
	want := map[string]string{
		"CODEX_HOME":               "/tmp/x/codex-home",
		"CODEX_EXEC_SERVERS_JSON":  "/tmp/x/exec_servers.json",
		"CODEX_EXEC_GATEWAY_TOKEN": "tokenX",
		"CODEX_API_KEY":            "wsTok",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s]: got %q want %q", k, env[k], v)
		}
	}
}

// fakeDriver satisfies the Driver interface for higher-level tests.
type fakeDriver struct {
	events []codex.ThreadEvent
	waited bool
	waitErr error
}

func (f *fakeDriver) Run(_ context.Context, _ RunInput) (DriverStream, error) {
	ch := make(chan codex.ThreadEvent, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return &fakeStream{ch: ch, parent: f}, nil
}

type fakeStream struct {
	ch     chan codex.ThreadEvent
	parent *fakeDriver
}

func (s *fakeStream) Events() <-chan codex.ThreadEvent { return s.ch }
func (s *fakeStream) Wait() error {
	s.parent.waited = true
	return s.parent.waitErr
}

func TestDriver_InterfaceShape(t *testing.T) {
	var _ Driver = (*SDKDriver)(nil)
	var _ Driver = (*fakeDriver)(nil)
}
```

- [ ] **Step 2: Run test, verify FAIL**

Run: `go test ./internal/codexappgateway/runner/ -run "TestSDKDriver|TestDriver_Interface" -v`
Expected: FAIL — `SDKDriver`, `Driver`, `RunInput` undefined.

- [ ] **Step 3: Implement the driver**

`internal/codexappgateway/runner/runner.go`:
```go
package runner

import (
	"context"
	"fmt"

	codex "github.com/agentserver/codex-agent-sdk-go"
)

// RunInput is everything one turn needs to spawn codex via the SDK.
// All fields are filled in by sessionWorker before calling Driver.Run.
type RunInput struct {
	ThreadID        string // empty → StartThread; non-empty → ResumeThread
	Input           codex.Input
	Model           string
	ProjectDir      string
	CodexHome       string
	ManifestPath    string
	CapToken        string
	WorkspaceTok    string
	OutputSchema    any
	ApprovalDefault codex.ApprovalMode // phase-1 callers must pass codex.ApprovalNever
}

// DriverStream is what one Driver.Run call returns. Mirrors codex.StreamedTurn
// but is interface-typed so fakes can substitute it in tests.
type DriverStream interface {
	Events() <-chan codex.ThreadEvent
	Wait() error
}

// Driver abstracts the SDK so the session worker is testable without
// spawning a real codex subprocess.
type Driver interface {
	Run(ctx context.Context, in RunInput) (DriverStream, error)
}

// SDKConfig is the driver's static config (set once at server start).
type SDKConfig struct {
	LLMProxyURL       string
	CodexPathOverride string
}

// SDKDriver is the production Driver, wrapping codex-agent-sdk-go.
type SDKDriver struct{ cfg SDKConfig }

func NewSDKDriver(cfg SDKConfig) *SDKDriver { return &SDKDriver{cfg: cfg} }

func (d *SDKDriver) Run(ctx context.Context, in RunInput) (DriverStream, error) {
	client := codex.New(codex.CodexOptions{
		CodexPathOverride: d.cfg.CodexPathOverride,
		BaseURL:           d.cfg.LLMProxyURL,
		APIKey:            in.WorkspaceTok,
		Env:               d.composeCodexEnv(in),
	})
	tOpts := d.threadOptions(in)
	var thread *codex.Thread
	if in.ThreadID == "" {
		thread = client.StartThread(tOpts)
	} else {
		thread = client.ResumeThread(in.ThreadID, tOpts)
	}
	stream, err := thread.RunStreamed(ctx, in.Input, codex.TurnOptions{
		OutputSchema: in.OutputSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("RunStreamed: %w", err)
	}
	// codex.StreamedTurn already exposes Events() <-chan ThreadEvent and Wait().
	return stream, nil
}

func (d *SDKDriver) threadOptions(in RunInput) codex.ThreadOptions {
	approval := in.ApprovalDefault
	if approval == "" {
		// Phase-1 invariant per spec § Non-goals.
		approval = codex.ApprovalNever
	}
	return codex.ThreadOptions{
		Model:            in.Model,
		SandboxMode:      codex.SandboxWorkspaceWrite,
		WorkingDirectory: in.ProjectDir,
		SkipGitRepoCheck: true,
		ApprovalPolicy:   approval,
	}
}

func (d *SDKDriver) composeCodexEnv(in RunInput) map[string]string {
	return map[string]string{
		"CODEX_HOME":               in.CodexHome,
		"CODEX_EXEC_SERVERS_JSON":  in.ManifestPath,
		"CODEX_EXEC_GATEWAY_TOKEN": in.CapToken,
		"CODEX_API_KEY":            in.WorkspaceTok,
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/codexappgateway/runner/ -run "TestSDKDriver|TestDriver_Interface" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/runner/runner.go internal/codexappgateway/runner/runner_test.go
git commit -m "feat(codex-app-gateway): SDK driver wires codex-agent-sdk-go (approval=never)"
```

---

## Task 7: Event mapper (`ThreadEvent` → `ServerNotification`)

**Files:**
- Create: `internal/codexappgateway/runner/event_mapper.go`
- Create: `internal/codexappgateway/runner/event_mapper_test.go`

A pure function. Table-driven. Returns a slice because some events
naturally fan out to two notifications (e.g., `ItemUpdatedEvent` of an
`AgentMessageItem` should emit a delta synthesized from the text growth
between updates — handled with a small per-item ratchet kept inside the
mapper struct, NOT a pure function).

- [ ] **Step 1: Write the failing tests**

`internal/codexappgateway/runner/event_mapper_test.go`:
```go
package runner

import (
	"strings"
	"testing"

	codex "github.com/agentserver/codex-agent-sdk-go"
	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
)

func encodedMethod(t *testing.T, n protocol.ServerNotification) string {
	t.Helper()
	method, _, err := n.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return method
}

func encodedParams(t *testing.T, n protocol.ServerNotification) []byte {
	t.Helper()
	_, body, err := n.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return body
}

func TestEventMapper_ThreadStarted(t *testing.T) {
	m := NewEventMapper("trn_1", "")
	got := m.Map(&codex.ThreadStartedEvent{Type: "thread.started", ThreadID: "thr_new"})
	if len(got) != 1 || encodedMethod(t, got[0]) != "thread/started" {
		t.Fatalf("notif: %+v", got)
	}
	if got[0].ThreadStarted == nil || got[0].ThreadStarted.Thread.ID != "thr_new" {
		t.Errorf("thread.id: %+v", got[0].ThreadStarted)
	}
}

func TestEventMapper_TurnStartedAndCompleted(t *testing.T) {
	m := NewEventMapper("trn_1", "thr_1")
	if got := m.Map(&codex.TurnStartedEvent{Type: "turn.started"}); encodedMethod(t, got[0]) != "turn/started" {
		t.Errorf("turn/started: %s", encodedMethod(t, got[0]))
	}
	got := m.Map(&codex.TurnCompletedEvent{
		Type:  "turn.completed",
		Usage: codex.Usage{InputTokens: 100, OutputTokens: 30},
	})
	if encodedMethod(t, got[0]) != "turn/completed" {
		t.Errorf("turn/completed: %s", encodedMethod(t, got[0]))
	}
	if got[0].TurnCompleted == nil || got[0].TurnCompleted.Usage.InputTokens != 100 {
		t.Errorf("usage missing: %+v", got[0].TurnCompleted)
	}
}

func TestEventMapper_ItemStartedAndCompleted_AgentMessage(t *testing.T) {
	m := NewEventMapper("trn_1", "thr_1")
	item := &codex.AgentMessageItem{ID: "itm_1", Type: "agent_message", Text: "hello"}
	starts := m.Map(&codex.ItemStartedEvent{Type: "item.started", Item: item})
	if encodedMethod(t, starts[0]) != "item/started" {
		t.Errorf("item/started: %s", encodedMethod(t, starts[0]))
	}
	completes := m.Map(&codex.ItemCompletedEvent{Type: "item.completed", Item: item})
	if encodedMethod(t, completes[0]) != "item/completed" {
		t.Errorf("item/completed: %s", encodedMethod(t, completes[0]))
	}
}

func TestEventMapper_ItemUpdated_AgentMessage_EmitsDelta(t *testing.T) {
	m := NewEventMapper("trn_1", "thr_1")
	first := &codex.AgentMessageItem{ID: "itm_1", Type: "agent_message", Text: "hel"}
	if got := m.Map(&codex.ItemUpdatedEvent{Type: "item.updated", Item: first}); encodedMethod(t, got[0]) != "item/agentMessage/delta" {
		t.Fatalf("first delta method: %s", encodedMethod(t, got[0]))
	}
	// Second update growing the text → delta is just the new suffix.
	second := &codex.AgentMessageItem{ID: "itm_1", Type: "agent_message", Text: "hello world"}
	got := m.Map(&codex.ItemUpdatedEvent{Type: "item.updated", Item: second})
	if got[0].AgentMessageDelta == nil ||
		got[0].AgentMessageDelta.ItemID != "itm_1" ||
		got[0].AgentMessageDelta.Delta != "lo world" {
		t.Errorf("delta: %+v", got[0].AgentMessageDelta)
	}
	// Wire-level sanity: encoded params must use camelCase keys.
	body := encodedParams(t, got[0])
	for _, want := range []string{`"itemId":`, `"turnId":`, `"delta":`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("encoded delta missing %s: %s", want, body)
		}
	}
}

func TestEventMapper_ItemUpdated_NonAgentMessage_DropsSilently(t *testing.T) {
	m := NewEventMapper("trn_1", "thr_1")
	got := m.Map(&codex.ItemUpdatedEvent{
		Type: "item.updated",
		Item: &codex.CommandExecutionItem{ID: "c1", Type: "command_execution"},
	})
	if len(got) != 0 {
		t.Errorf("want drop, got %d notifs", len(got))
	}
}

func TestEventMapper_ThreadError(t *testing.T) {
	m := NewEventMapper("trn_1", "thr_1")
	got := m.Map(&codex.ThreadErrorEvent{Type: "error", Message: "boom"})
	if encodedMethod(t, got[0]) != "error" {
		t.Errorf("method: %s", encodedMethod(t, got[0]))
	}
}

func TestEventMapper_TurnFailed_IsErrorNotification(t *testing.T) {
	m := NewEventMapper("trn_1", "thr_1")
	got := m.Map(&codex.TurnFailedEvent{Type: "turn.failed", Error: codex.ThreadError{Message: "denied"}})
	if encodedMethod(t, got[0]) != "error" {
		t.Errorf("method: %s", encodedMethod(t, got[0]))
	}
}

func TestEventMapper_UnknownEvent_Drops(t *testing.T) {
	m := NewEventMapper("trn_1", "thr_1")
	got := m.Map(&codex.UnknownEvent{Type: "something_new"})
	if len(got) != 0 {
		t.Errorf("want drop, got %d", len(got))
	}
}

var _ = protocol.ServerNotification{} // ensure import survives if all asserts removed
```

- [ ] **Step 2: Run tests, verify FAIL**

Run: `go test ./internal/codexappgateway/runner/ -run TestEventMapper -v`
Expected: FAIL — `NewEventMapper` undefined.

- [ ] **Step 3: Implement the mapper**

`internal/codexappgateway/runner/event_mapper.go`:
```go
package runner

import (
	"strings"

	codex "github.com/agentserver/codex-agent-sdk-go"
	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
)

// EventMapper turns codex SDK ThreadEvents into the gateway's
// ServerNotification union. It carries per-item state so that
// AgentMessage updates can be diffed into incremental deltas
// (item/agentMessage/delta), since codex item.updated events
// carry the full text not the tail.
type EventMapper struct {
	turnID       string
	threadID     string
	agentMsgSeen map[string]string // item_id → last-seen text
}

func NewEventMapper(turnID, threadID string) *EventMapper {
	return &EventMapper{
		turnID:       turnID,
		threadID:     threadID,
		agentMsgSeen: map[string]string{},
	}
}

// Map returns zero or more ServerNotifications for the given event.
// Pure with respect to its receiver state (which is single-goroutine
// per turn, written only by the worker's pump loop). Each branch
// constructs a typed `protocol.ServerNotification` variant directly so
// the union's `Encode()` (defined in 2a) emits the correct method +
// camelCase JSON params. There is intentionally no untyped helper that
// takes (method, body) — that shape silently bypassed the sum-type and
// caused snake_case keys to leak onto the wire (see post-recon audit
// P0-1).
func (m *EventMapper) Map(evt codex.ThreadEvent) []protocol.ServerNotification {
	switch e := evt.(type) {
	case *codex.ThreadStartedEvent:
		if m.threadID == "" {
			m.threadID = e.ThreadID
		}
		return []protocol.ServerNotification{{
			ThreadStarted: &protocol.ThreadStartedParams{
				Thread: protocol.Thread{ID: e.ThreadID},
			},
		}}

	case *codex.TurnStartedEvent:
		return []protocol.ServerNotification{{
			TurnStarted: &protocol.TurnStartedParams{
				ThreadID: m.threadID,
				Turn:     protocol.Turn{ID: m.turnID, Status: protocol.TurnInProgress, Items: []protocol.ThreadItem{}},
			},
		}}

	case *codex.TurnCompletedEvent:
		usage := protocol.Usage{
			InputTokens:           e.Usage.InputTokens,
			CachedInputTokens:     e.Usage.CachedInputTokens,
			OutputTokens:          e.Usage.OutputTokens,
			ReasoningOutputTokens: e.Usage.ReasoningOutputTokens,
		}
		return []protocol.ServerNotification{{
			TurnCompleted: &protocol.TurnCompletedParams{
				ThreadID: m.threadID,
				Turn:     protocol.Turn{ID: m.turnID, Status: protocol.TurnCompleted, Items: []protocol.ThreadItem{}, Usage: &usage},
			},
		}}

	case *codex.TurnFailedEvent:
		return []protocol.ServerNotification{{
			Error: &protocol.ErrorParams{
				ThreadID:  m.threadID,
				TurnID:    m.turnID,
				WillRetry: false,
				Error:     protocol.ThreadError{Message: e.Error.Message},
			},
		}}

	case *codex.ItemStartedEvent:
		pi := translateItem(e.Item)
		if pi == nil {
			return nil
		}
		return []protocol.ServerNotification{{
			ItemStarted: &protocol.ItemEnvelope{ThreadID: m.threadID, TurnID: m.turnID, Item: pi},
		}}

	case *codex.ItemUpdatedEvent:
		// Only AgentMessage updates produce a streamable delta; everything
		// else is dropped — clients see the final value at item.completed.
		if am, ok := e.Item.(*codex.AgentMessageItem); ok {
			prev := m.agentMsgSeen[am.ID]
			delta := am.Text
			if strings.HasPrefix(am.Text, prev) {
				delta = am.Text[len(prev):]
			}
			m.agentMsgSeen[am.ID] = am.Text
			if delta == "" {
				return nil
			}
			return []protocol.ServerNotification{{
				AgentMessageDelta: &protocol.AgentMessageDeltaParams{
					ThreadID: m.threadID,
					TurnID:   m.turnID,
					ItemID:   am.ID,
					Delta:    delta,
				},
			}}
		}
		return nil

	case *codex.ItemCompletedEvent:
		pi := translateItem(e.Item)
		if pi == nil {
			return nil
		}
		return []protocol.ServerNotification{{
			ItemCompleted: &protocol.ItemEnvelope{ThreadID: m.threadID, TurnID: m.turnID, Item: pi},
		}}

	case *codex.ThreadErrorEvent:
		return []protocol.ServerNotification{{
			Error: &protocol.ErrorParams{
				ThreadID:  m.threadID,
				TurnID:    m.turnID,
				WillRetry: false,
				Error:     protocol.ThreadError{Message: e.Message},
			},
		}}

	case *codex.UnknownEvent:
		// Drop; logging happens at the worker layer with full slog.
		return nil
	}
	return nil
}

// translateItem converts an SDK codex.ThreadItem into the gateway's
// protocol.ThreadItem. Two distinct interfaces (sealed by different
// itemSeal()s in their respective packages) so a direct assignment
// won't compile — translation happens here. Unknown / unrepresentable
// variants return nil and the caller drops the event (parity with the
// gateway's strict DecodeThreadItem policy).
func translateItem(in codex.ThreadItem) protocol.ThreadItem {
	switch v := in.(type) {
	case *codex.AgentMessageItem:
		return &protocol.AgentMessageItem{ID: v.ID, Type: "agentMessage", Text: v.Text}
	case *codex.ReasoningItem:
		return &protocol.ReasoningItem{ID: v.ID, Type: "reasoning", Text: v.Text}
	case *codex.CommandExecutionItem:
		out := &protocol.CommandExecutionItem{
			ID:               v.ID,
			Type:             "commandExecution",
			Command:          v.Command,
			AggregatedOutput: v.AggregatedOutput,
			Status:           v.Status,
		}
		if v.ExitCode != nil {
			ec := *v.ExitCode
			out.ExitCode = &ec
		}
		return out
	case *codex.FileChangeItem:
		changes := make([]protocol.FileUpdateChange, 0, len(v.Changes))
		for _, c := range v.Changes {
			changes = append(changes, protocol.FileUpdateChange{Path: c.Path, Kind: c.Kind})
		}
		return &protocol.FileChangeItem{ID: v.ID, Type: "fileChange", Changes: changes, Status: v.Status}
	case *codex.McpToolCallItem:
		return &protocol.McpToolCallItem{
			ID:        v.ID,
			Type:      "mcpToolCall",
			Server:    v.Server,
			Tool:      v.Tool,
			Arguments: v.Arguments,
			Status:    v.Status,
		}
	case *codex.WebSearchItem:
		return &protocol.WebSearchItem{ID: v.ID, Type: "webSearch", Query: v.Query}
	case *codex.TodoListItem:
		entries := make([]protocol.TodoEntry, 0, len(v.Items))
		for _, e := range v.Items {
			entries = append(entries, protocol.TodoEntry{Text: e.Text, Completed: e.Completed})
		}
		return &protocol.TodoListItem{ID: v.ID, Type: "todoList", Items: entries}
	case *codex.ErrorItem:
		return &protocol.ErrorItem{ID: v.ID, Type: "error", Message: v.Message}
	case *codex.UnknownItem:
		// Strict policy: drop. Worker logs the raw payload at INFO before this point.
		return nil
	}
	return nil
}
```

Add a unit test that exercises every translation branch:

```go
// internal/codexappgateway/runner/event_mapper_test.go (append)

func TestTranslateItem_AllVariants(t *testing.T) {
	exit := 0
	cases := []struct {
		name string
		in   codex.ThreadItem
		want string // expected protocol.ThreadItem.ItemType()
	}{
		{"agentMessage", &codex.AgentMessageItem{ID: "a", Text: "x"}, "agentMessage"},
		{"reasoning", &codex.ReasoningItem{ID: "r", Text: "t"}, "reasoning"},
		{"commandExecution", &codex.CommandExecutionItem{ID: "c", Command: "ls", ExitCode: &exit, Status: "completed"}, "commandExecution"},
		{"fileChange", &codex.FileChangeItem{ID: "f", Status: "completed"}, "fileChange"},
		{"mcpToolCall", &codex.McpToolCallItem{ID: "m", Server: "s", Tool: "t", Status: "completed"}, "mcpToolCall"},
		{"webSearch", &codex.WebSearchItem{ID: "w", Query: "go"}, "webSearch"},
		{"todoList", &codex.TodoListItem{ID: "t"}, "todoList"},
		{"error", &codex.ErrorItem{ID: "e", Message: "boom"}, "error"},
	}
	for _, c := range cases {
		out := translateItem(c.in)
		if out == nil {
			t.Errorf("%s: translateItem returned nil", c.name)
			continue
		}
		if out.ItemType() != c.want {
			t.Errorf("%s: ItemType()=%q want %q", c.name, out.ItemType(), c.want)
		}
	}
	if got := translateItem(&codex.UnknownItem{Type: "future"}); got != nil {
		t.Errorf("UnknownItem: want nil, got %T", got)
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/codexappgateway/runner/ -run TestEventMapper -v`
Expected: PASS for all 8 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/runner/event_mapper.go internal/codexappgateway/runner/event_mapper_test.go
git commit -m "feat(codex-app-gateway): SDK event → ServerNotification mapper (with deltas)"
```

---

## Task 8: Session worker (per-thread serialized queue)

**Files:**
- Create: `internal/codexappgateway/session_worker.go`
- Create: `internal/codexappgateway/worker_registry.go`
- Create: `internal/codexappgateway/session_worker_test.go`

The worker mirrors `internal/ccbroker/session_worker.go` but speaks
notifications instead of SSE events and persists each emitted
`ServerNotification` to `codex_turn_events`. Per-turn `defer` cleans up
the manifest tmp dir, the workspace, and posts the revoke regardless of
exit path. Cancel funcs live on the `WorkerRegistry`.

- [ ] **Step 1: Write the failing test**

`internal/codexappgateway/session_worker_test.go`:
```go
package codexappgateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	codex "github.com/agentserver/codex-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/codexappgateway/runner"
	"github.com/agentserver/agentserver/internal/codexappgateway/store"
)

type memStore struct {
	mu       sync.Mutex
	pending  []*store.AgentTurn
	running  map[string]bool
	done     map[string]bool
	failed   map[string]string
	canc     map[string]bool
	events   map[string][]json.RawMessage
}

func newMemStore() *memStore {
	return &memStore{
		running: map[string]bool{}, done: map[string]bool{},
		failed: map[string]string{}, canc: map[string]bool{},
		events: map[string][]json.RawMessage{},
	}
}
func (s *memStore) PickNextPending(_ context.Context, threadID string) (*store.AgentTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.pending {
		if t.ThreadID == threadID {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			s.running[t.ID] = true
			return t, nil
		}
	}
	return nil, nil
}
func (s *memStore) MarkTurnRunning(_ context.Context, id string) error  { return nil }
func (s *memStore) MarkTurnDone(_ context.Context, id string) error     { s.mu.Lock(); defer s.mu.Unlock(); s.done[id] = true; return nil }
func (s *memStore) MarkTurnFailed(_ context.Context, id, msg string) error {
	s.mu.Lock(); defer s.mu.Unlock(); s.failed[id] = msg; return nil
}
func (s *memStore) MarkTurnCancelled(_ context.Context, id string) error {
	s.mu.Lock(); defer s.mu.Unlock(); s.canc[id] = true; return nil
}
func (s *memStore) InsertEvent(_ context.Context, turnID string, payload json.RawMessage) (int64, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.events[turnID] = append(s.events[turnID], payload)
	return int64(len(s.events[turnID])), nil
}

type fakeWorkspace struct{ tmpRoot string }

func (w *fakeWorkspace) Setup(_ context.Context, _, threadID string) (WorkspaceLayout, error) {
	return WorkspaceLayout{
		CodexHome: w.tmpRoot + "/" + threadID + "/codex-home",
		ProjectDir: w.tmpRoot + "/" + threadID + "/proj",
	}, nil
}
func (w *fakeWorkspace) Teardown(_ context.Context, _ WorkspaceLayout) error { return nil }

type fakeManifest struct{ called int }

func (f *fakeManifest) BuildAndWrite(_ context.Context, in runner.BuildInput) (runner.BuildResult, error) {
	f.called++
	return runner.BuildResult{
		ManifestPath: "/tmp/x/exec_servers.json",
		CapToken:     "tok",
		ExeIDs:       []string{"exe_alpha"},
	}, nil
}

type fakeBroadcaster struct {
	mu        sync.Mutex
	pushed    []json.RawMessage
}

func (b *fakeBroadcaster) Push(_ string, n json.RawMessage) {
	b.mu.Lock(); defer b.mu.Unlock()
	b.pushed = append(b.pushed, n)
}

type fakeRevoker struct{ mu sync.Mutex; ids []string }

func (r *fakeRevoker) Revoke(_ context.Context, turnID string) {
	r.mu.Lock(); defer r.mu.Unlock(); r.ids = append(r.ids, turnID)
}

type fakeDriver struct{ events []codex.ThreadEvent; err error }

func (d *fakeDriver) Run(_ context.Context, _ runner.RunInput) (runner.DriverStream, error) {
	if d.err != nil { return nil, d.err }
	ch := make(chan codex.ThreadEvent, len(d.events))
	for _, e := range d.events { ch <- e }
	close(ch)
	return &fdStream{ch: ch}, nil
}

type fdStream struct{ ch chan codex.ThreadEvent }

func (s *fdStream) Events() <-chan codex.ThreadEvent { return s.ch }
func (s *fdStream) Wait() error                       { return nil }

func TestSessionWorker_HappyPath_PersistsAndPushesAndRevokes(t *testing.T) {
	st := newMemStore()
	turn := &store.AgentTurn{
		ID: "trn_1", ThreadID: "thr_1", WorkspaceID: "ws_1",
		UserInput: json.RawMessage(`[{"type":"text","text":"hi"}]`),
		Status:    "pending",
		EnqueuedAt: time.Now(),
	}
	st.pending = append(st.pending, turn)

	rev := &fakeRevoker{}
	bcast := &fakeBroadcaster{}
	deps := WorkerDeps{
		Store:        st,
		Workspace:    &fakeWorkspace{tmpRoot: t.TempDir()},
		Manifest:     &fakeManifest{},
		Driver:       &fakeDriver{events: []codex.ThreadEvent{
			&codex.ThreadStartedEvent{Type: "thread.started", ThreadID: "thr_1"},
			&codex.TurnStartedEvent{Type: "turn.started"},
			&codex.TurnCompletedEvent{Type: "turn.completed"},
		}},
		Broadcaster:  bcast,
		Revoker:      rev,
		WSToken:      func(_ context.Context, _ string) (string, error) { return "wsTok", nil },
		Logger:       discardLogger(),
		TmpRoot:      t.TempDir(),
	}
	w := newSessionWorker("thr_1", deps, nil)
	w.executeFn = w.execute

	// Run one tick then quit.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	w.Notify()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if !st.done["trn_1"] {
		t.Errorf("turn not marked done: failed=%v cancelled=%v", st.failed, st.canc)
	}
	if len(st.events["trn_1"]) != 3 {
		t.Errorf("events: got %d want 3", len(st.events["trn_1"]))
	}
	if len(bcast.pushed) != 3 {
		t.Errorf("pushed: got %d want 3", len(bcast.pushed))
	}
	if len(rev.ids) != 1 || rev.ids[0] != "trn_1" {
		t.Errorf("revoke: %v", rev.ids)
	}
}

func TestSessionWorker_DriverError_MarksFailed_StillRevokes(t *testing.T) {
	st := newMemStore()
	st.pending = append(st.pending, &store.AgentTurn{
		ID: "trn_2", ThreadID: "thr_1", WorkspaceID: "ws_1",
		UserInput: json.RawMessage(`[]`), Status: "pending", EnqueuedAt: time.Now(),
	})
	rev := &fakeRevoker{}
	deps := WorkerDeps{
		Store:       st,
		Workspace:   &fakeWorkspace{tmpRoot: t.TempDir()},
		Manifest:    &fakeManifest{},
		Driver:      &fakeDriver{err: errors.New("spawn failed")},
		Broadcaster: &fakeBroadcaster{},
		Revoker:     rev,
		WSToken:     func(_ context.Context, _ string) (string, error) { return "tok", nil },
		Logger:      discardLogger(),
		TmpRoot:     t.TempDir(),
	}
	w := newSessionWorker("thr_1", deps, nil)
	w.executeFn = w.execute
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	w.Notify()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if _, bad := st.failed["trn_2"]; !bad {
		t.Errorf("want failed, got: %+v", st.failed)
	}
	if len(rev.ids) != 1 {
		t.Errorf("want 1 revoke, got %d", len(rev.ids))
	}
}
```

Add small helper at top of file:
```go
import "log/slog"
import "io"
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
```

- [ ] **Step 2: Run tests, verify FAIL**

Run: `go test ./internal/codexappgateway/ -run TestSessionWorker -v`
Expected: FAIL — types undefined.

- [ ] **Step 3: Implement worker registry**

`internal/codexappgateway/worker_registry.go`:
```go
package codexappgateway

import (
	"context"
	"sync"
)

// WorkerRegistry holds per-thread sessionWorker handles, plus the
// active turn cancel functions used by `turn/interrupt`.
type WorkerRegistry struct {
	mu       sync.Mutex
	workers  map[string]*sessionWorker
	cancels  map[string]map[string]context.CancelFunc // threadID → turnID → cancel
	makeWorker func(threadID string) *sessionWorker
}

func NewWorkerRegistry(make func(threadID string) *sessionWorker) *WorkerRegistry {
	return &WorkerRegistry{
		workers:    map[string]*sessionWorker{},
		cancels:    map[string]map[string]context.CancelFunc{},
		makeWorker: make,
	}
}

// Notify wakes (or starts) the worker for threadID.
func (r *WorkerRegistry) Notify(threadID string) {
	r.mu.Lock()
	w, ok := r.workers[threadID]
	if !ok {
		w = r.makeWorker(threadID)
		r.workers[threadID] = w
		go w.run(context.Background())
	}
	r.mu.Unlock()
	w.Notify()
}

// Cancel cancels a specific running turn. Returns true if a cancel was
// fired (i.e., the turn is currently running on this Pod).
func (r *WorkerRegistry) Cancel(threadID, turnID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.cancels[threadID]
	if !ok {
		return false
	}
	c, ok := m[turnID]
	if !ok {
		return false
	}
	c()
	return true
}

func (r *WorkerRegistry) setCancel(threadID, turnID string, c context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cancels[threadID]; !ok {
		r.cancels[threadID] = map[string]context.CancelFunc{}
	}
	r.cancels[threadID][turnID] = c
}

func (r *WorkerRegistry) clearCancel(threadID, turnID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.cancels[threadID]; ok {
		delete(m, turnID)
		if len(m) == 0 {
			delete(r.cancels, threadID)
		}
	}
}

// idleEvict removes a worker from the registry (used by sessionWorker on idle exit).
func (r *WorkerRegistry) idleEvict(threadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.workers, threadID)
}
```

- [ ] **Step 4: Implement the session worker**

`internal/codexappgateway/session_worker.go`:
```go
package codexappgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	codex "github.com/agentserver/codex-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
	"github.com/agentserver/agentserver/internal/codexappgateway/runner"
	"github.com/agentserver/agentserver/internal/codexappgateway/store"
)

const defaultWorkerIdleTimeout = 5 * time.Minute

// WorkspaceLayout is the per-turn directory layout produced by the
// workspace package. Re-exported here as an interface seam so tests
// can fake it without depending on agentworkspace internals.
type WorkspaceLayout struct {
	CodexHome  string
	ProjectDir string
}

// Workspace abstracts the agentworkspace package for tests.
type Workspace interface {
	Setup(ctx context.Context, workspaceID, threadID string) (WorkspaceLayout, error)
	Teardown(ctx context.Context, layout WorkspaceLayout) error
}

// ManifestBuilder abstracts the manifest writer.
type ManifestBuilder interface {
	BuildAndWrite(ctx context.Context, in runner.BuildInput) (runner.BuildResult, error)
}

// Broadcaster pushes ServerNotifications to whichever ws connection
// currently subscribes to threadID. Implementation lives in connregistry
// (Plan 2a).
type Broadcaster interface {
	Push(threadID string, payload json.RawMessage)
}

// Revoker fires the terminal-state revocation POST. Implementation in revoke.go.
type Revoker interface {
	Revoke(ctx context.Context, turnID string)
}

// WorkerStore is the per-turn DB seam.
type WorkerStore interface {
	PickNextPending(ctx context.Context, threadID string) (*store.AgentTurn, error)
	MarkTurnRunning(ctx context.Context, id string) error
	MarkTurnDone(ctx context.Context, id string) error
	MarkTurnFailed(ctx context.Context, id, msg string) error
	MarkTurnCancelled(ctx context.Context, id string) error
	InsertEvent(ctx context.Context, turnID string, payload json.RawMessage) (int64, error)
}

type WorkerDeps struct {
	Store       WorkerStore
	Workspace   Workspace
	Manifest    ManifestBuilder
	Driver      runner.Driver
	Broadcaster Broadcaster
	Revoker     Revoker
	WSToken     func(ctx context.Context, workspaceID string) (string, error)
	Logger      *slog.Logger
	TmpRoot     string
	Registry    *WorkerRegistry // for cancel func registration
}

type sessionWorker struct {
	threadID  string
	deps      WorkerDeps
	wake      chan struct{}
	quit      chan struct{}
	idleAfter time.Duration
	executeFn func(ctx context.Context, t *store.AgentTurn)
	onIdle    func(threadID string)
}

func newSessionWorker(threadID string, deps WorkerDeps, onIdle func(string)) *sessionWorker {
	w := &sessionWorker{
		threadID:  threadID,
		deps:      deps,
		wake:      make(chan struct{}, 1),
		quit:      make(chan struct{}),
		idleAfter: defaultWorkerIdleTimeout,
		onIdle:    onIdle,
	}
	w.executeFn = w.execute
	return w
}

func (w *sessionWorker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *sessionWorker) run(ctx context.Context) {
	idle := time.NewTimer(w.idleAfter)
	defer idle.Stop()
	for {
		turn, err := w.deps.Store.PickNextPending(ctx, w.threadID)
		if err != nil {
			w.deps.Logger.Error("pick pending", "thread_id", w.threadID, "error", err)
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
			resetTimer(idle, w.idleAfter)
			continue
		}
		select {
		case <-w.wake:
			resetTimer(idle, w.idleAfter)
		case <-idle.C:
			if w.onIdle != nil {
				w.onIdle(w.threadID)
			}
			return
		case <-w.quit:
			return
		case <-ctx.Done():
			return
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// execute owns one turn end-to-end. Cleanup is fully defer-driven so
// success / fail / cancel all release manifests, workspace, and POST
// revoke.
func (w *sessionWorker) execute(ctx context.Context, turn *store.AgentTurn) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if w.deps.Registry != nil {
		w.deps.Registry.setCancel(turn.ThreadID, turn.ID, cancel)
		defer w.deps.Registry.clearCancel(turn.ThreadID, turn.ID)
	}

	// Always revoke the cap token on terminal state. Fire-and-forget; uses
	// background context so a cancelled turn still revokes promptly.
	defer w.deps.Revoker.Revoke(context.Background(), turn.ID)
	defer runner.CleanupManifest(turn.ID, w.deps.TmpRoot)

	// Emit thread/status/changed=running as soon as the worker takes
	// ownership of the turn. This is lifecycle metadata pushed to live
	// connections only — it is NOT persisted to codex_turn_events.
	w.publishStatus(turn.ThreadID, "running")
	if err := w.deps.Store.MarkTurnRunning(ctx, turn.ID); err != nil {
		w.fail(turn, "mark running: "+err.Error())
		return
	}
	if turnCtx.Err() != nil {
		_ = w.deps.Store.MarkTurnCancelled(context.Background(), turn.ID)
		w.publishTerminal(turn, "cancelled", "")
		return
	}

	wsTok, err := w.deps.WSToken(ctx, turn.WorkspaceID)
	if err != nil {
		w.fail(turn, "wstoken: "+err.Error())
		return
	}
	layout, err := w.deps.Workspace.Setup(ctx, turn.WorkspaceID, turn.ThreadID)
	if err != nil {
		w.fail(turn, "workspace setup: "+err.Error())
		return
	}
	defer func() { _ = w.deps.Workspace.Teardown(context.Background(), layout) }()

	mres, err := w.deps.Manifest.BuildAndWrite(ctx, runner.BuildInput{
		TurnID:      turn.ID,
		WorkspaceID: turn.WorkspaceID,
	})
	if err != nil {
		w.fail(turn, "manifest: "+err.Error())
		return
	}

	// Decode optional per-turn options out of turn.TurnOptions.
	var opts struct {
		Model        string          `json:"model"`
		OutputSchema json.RawMessage `json:"output_schema"`
	}
	if len(turn.TurnOptions) > 0 {
		_ = json.Unmarshal(turn.TurnOptions, &opts)
	}
	var outputSchema any
	if len(opts.OutputSchema) > 0 {
		_ = json.Unmarshal(opts.OutputSchema, &outputSchema)
	}

	stream, err := w.deps.Driver.Run(turnCtx, runner.RunInput{
		ThreadID:        turn.ThreadID,
		Input:           decodeInput(turn.UserInput),
		Model:           opts.Model,
		ProjectDir:      layout.ProjectDir,
		CodexHome:       layout.CodexHome,
		ManifestPath:    mres.ManifestPath,
		CapToken:        mres.CapToken,
		WorkspaceTok:    wsTok,
		OutputSchema:    outputSchema,
		ApprovalDefault: codex.ApprovalNever,
	})
	if err != nil {
		w.fail(turn, "driver.Run: "+err.Error())
		return
	}

	mapper := runner.NewEventMapper(turn.ID, turn.ThreadID)
	for evt := range stream.Events() {
		for _, n := range mapper.Map(evt) {
			payload, err := encodeNotification(n)
			if err != nil {
				w.deps.Logger.Warn("ServerNotification.Encode failed", "turn_id", turn.ID, "error", err)
				continue
			}
			if _, err := w.deps.Store.InsertEvent(context.Background(), turn.ID, payload); err != nil {
				w.deps.Logger.Warn("InsertEvent failed", "turn_id", turn.ID, "error", err)
			}
			w.deps.Broadcaster.Push(turn.ThreadID, payload)
		}
	}
	if err := stream.Wait(); err != nil {
		w.fail(turn, "stream.Wait: "+err.Error())
		return
	}
	if turnCtx.Err() != nil {
		_ = w.deps.Store.MarkTurnCancelled(context.Background(), turn.ID)
		w.publishTerminal(turn, "cancelled", "")
		return
	}
	_ = w.deps.Store.MarkTurnDone(context.Background(), turn.ID)
	w.publishTerminal(turn, "done", "")
}

func (w *sessionWorker) fail(turn *store.AgentTurn, msg string) {
	w.deps.Logger.Error("turn failed", "turn_id", turn.ID, "error", msg)
	_ = w.deps.Store.MarkTurnFailed(context.Background(), turn.ID, msg)
	w.publishTerminal(turn, "failed", msg)
}

func (w *sessionWorker) publishTerminal(turn *store.AgentTurn, kind, msg string) {
	// Build the appropriate typed `protocol.ServerNotification` variant for
	// the terminal kind. `done` and `cancelled` map to `turn/completed`;
	// `failed` maps to `error` (since codex itself emits a TurnFailedEvent
	// translated to `error` upstream of this hook). All variants encode
	// camelCase params via the sum-type's Encode() — never hand-marshal
	// the wire body via map[string]any (see post-recon audit P0-1: that
	// shape silently bypassed the sum-type and leaked snake_case keys).
	var n protocol.ServerNotification
	switch kind {
	case "failed":
		n = protocol.ServerNotification{
			Error: &protocol.ErrorParams{
				ThreadID:  turn.ThreadID,
				TurnID:    turn.ID,
				WillRetry: false,
				Error:     protocol.ThreadError{Message: msg},
			},
		}
	default: // "done" | "cancelled" (internal labels; "cancelled"
		// becomes the wire/DB value "interrupted" via TurnInterrupted —
		// codex's terminology has no "cancelled", only "interrupted").
		status := protocol.TurnCompleted
		if kind == "cancelled" {
			status = protocol.TurnInterrupted
		}
		n = protocol.ServerNotification{
			TurnCompleted: &protocol.TurnCompletedParams{
				ThreadID: turn.ThreadID,
				Turn:     protocol.Turn{ID: turn.ID, Status: status, Items: []protocol.ThreadItem{}},
			},
		}
	}
	body, err := encodeNotification(n)
	if err != nil {
		w.deps.Logger.Warn("publishTerminal encode failed", "turn_id", turn.ID, "error", err)
		return
	}
	w.deps.Broadcaster.Push(turn.ThreadID, body)
	// Mirror the terminal exit as a thread/status/changed lifecycle
	// notification. Failed turns surface as `errored`; done/cancelled
	// both return the thread to `idle`.
	status := "idle"
	if kind == "failed" {
		status = "errored"
	}
	w.publishStatus(turn.ThreadID, status)
}

// publishStatus broadcasts a `thread/status/changed` lifecycle
// notification keyed by threadID. The envelope is pushed to live
// connections via the broadcaster but deliberately NOT persisted to
// `codex_turn_events` — it reflects worker state, not codex history,
// and is reconstructable from `codex_turns.status` on resume. Goes
// through the typed sum-type `Encode()` like every other ServerNotification
// site (no bespoke map[string]any envelope).
func (w *sessionWorker) publishStatus(threadID, status string) {
	n := protocol.ServerNotification{
		ThreadStatusChanged: &protocol.ThreadStatusChangedParams{
			ThreadID: threadID,
			Status:   status,
		},
	}
	body, err := encodeNotification(n)
	if err != nil {
		w.deps.Logger.Warn("publishStatus encode failed", "thread_id", threadID, "error", err)
		return
	}
	w.deps.Broadcaster.Push(threadID, body)
}

// encodeNotification wraps a typed ServerNotification in the canonical
// JSON-RPC envelope `{jsonrpc, method, params}`. Single helper, single
// place where the envelope shape lives — so the worker, the broadcaster,
// and tests all agree.
func encodeNotification(n protocol.ServerNotification) ([]byte, error) {
	method, params, err := n.Encode()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{JSONRPC: "2.0", Method: method, Params: params})
}

// decodeInput converts a raw JSON `input` payload (an array of UserInput)
// into the SDK's PartsInput value. Falls back to StringInput("") on
// unrecognized shape so the worker still produces a stream with a turn.failed.
func decodeInput(raw json.RawMessage) codex.Input {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil && len(parts) > 0 {
		out := make(codex.PartsInput, 0, len(parts))
		for _, p := range parts {
			switch p.Type {
			case "text":
				out = append(out, codex.UserInput{Type: codex.InputText, Text: p.Text})
			case "local_image":
				out = append(out, codex.UserInput{Type: codex.InputLocalImage, Path: p.Path})
			}
		}
		return out
	}
	// Fallback: bare string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return codex.StringInput(s)
	}
	return codex.StringInput("")
}

// Compile-time assertion that we are using fmt for nothing else.
var _ = fmt.Sprintf
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/codexappgateway/ -run TestSessionWorker -v`
Expected: PASS for both happy + driver-error cases.

- [ ] **Step 6: Commit**

```bash
git add internal/codexappgateway/session_worker.go internal/codexappgateway/worker_registry.go internal/codexappgateway/session_worker_test.go
git commit -m "feat(codex-app-gateway): per-thread session worker (manifest → driver → pump → revoke)"
```

---

## Task 9: Recovery on startup

**Files:**
- Create: `internal/codexappgateway/recovery.go`
- Create: `internal/codexappgateway/recovery_test.go`

Mirrors `internal/ccbroker/recovery.go`. Two store calls + a fan-out
notify; logs the count.

- [ ] **Step 1: Write the failing test**

`internal/codexappgateway/recovery_test.go`:
```go
package codexappgateway

import (
	"context"
	"sync"
	"testing"
)

type recovStore struct {
	resetN  int
	pending []string
}

func (s *recovStore) ResetRunningToQueued(_ context.Context) (int, error) {
	return s.resetN, nil
}
func (s *recovStore) ListThreadsWithPending(_ context.Context) ([]string, error) {
	return s.pending, nil
}

type recordingNotifier struct {
	mu sync.Mutex
	ids []string
}

func (n *recordingNotifier) Notify(threadID string) {
	n.mu.Lock(); defer n.mu.Unlock()
	n.ids = append(n.ids, threadID)
}

func TestRecover_ResetsAndNotifies(t *testing.T) {
	st := &recovStore{resetN: 3, pending: []string{"thr_a", "thr_b"}}
	n := &recordingNotifier{}
	if err := Recover(context.Background(), st, n, discardLogger()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(n.ids) != 2 {
		t.Errorf("notified: got %d want 2", len(n.ids))
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

Run: `go test ./internal/codexappgateway/ -run TestRecover -v`
Expected: FAIL — `Recover` undefined.

- [ ] **Step 3: Implement**

`internal/codexappgateway/recovery.go`:
```go
package codexappgateway

import (
	"context"
	"fmt"
	"log/slog"
)

// RecoveryStore is the subset of store needed for startup recovery.
type RecoveryStore interface {
	ResetRunningToQueued(ctx context.Context) (int, error)
	ListThreadsWithPending(ctx context.Context) ([]string, error)
}

// RecoveryNotifier mirrors the WorkerRegistry's Notify method only.
type RecoveryNotifier interface {
	Notify(threadID string)
}

// Recover is called once at server startup before HTTP serving begins.
// It resets stale `running` turns from a crashed prior pod back to
// `queued`, then pings the per-thread session worker for every thread
// that has any pending or queued work so the queue starts draining
// without waiting for fresh client traffic.
func Recover(ctx context.Context, s RecoveryStore, n RecoveryNotifier, logger *slog.Logger) error {
	count, err := s.ResetRunningToQueued(ctx)
	if err != nil {
		return fmt.Errorf("reset running→queued: %w", err)
	}
	if count > 0 {
		logger.Info("recovery: reset stale running turns", "count", count)
	}
	threads, err := s.ListThreadsWithPending(ctx)
	if err != nil {
		return fmt.Errorf("list threads with pending: %w", err)
	}
	for _, tid := range threads {
		n.Notify(tid)
	}
	logger.Info("recovery: notified workers", "thread_count", len(threads))
	return nil
}
```

- [ ] **Step 4: Run, verify PASS**

Run: `go test ./internal/codexappgateway/ -run TestRecover -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/recovery.go internal/codexappgateway/recovery_test.go
git commit -m "feat(codex-app-gateway): startup recovery (reset stale running, ping workers)"
```

---

## Task 10: Revocation push to exec-gateway

**Files:**
- Create: `internal/codexappgateway/revoke.go`
- Create: `internal/codexappgateway/revoke_test.go`

Per spec § Open risks #4: on terminal state, fan out a POST to every
configured exec-gateway URL. Fire-and-forget, with a 2-second timeout
per call so a slow replica cannot stall the worker. Failures are logged
but never returned.

- [ ] **Step 1: Write the failing test**

`internal/codexappgateway/revoke_test.go`:
```go
package codexappgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRevoker_FansOutToAllReplicas(t *testing.T) {
	var hitsA, hitsB int32
	mkSrv := func(counter *int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/exec-gateway/revoke-turn" {
				http.NotFound(w, r); return
			}
			body, _ := io.ReadAll(r.Body)
			var p struct{ TurnID string `json:"turn_id"` }
			_ = json.Unmarshal(body, &p)
			if p.TurnID != "trn_x" {
				t.Errorf("turn_id: %s", p.TurnID)
			}
			if r.Header.Get("Authorization") != "Bearer s" {
				t.Errorf("auth: %s", r.Header.Get("Authorization"))
			}
			atomic.AddInt32(counter, 1)
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	a := mkSrv(&hitsA); defer a.Close()
	b := mkSrv(&hitsB); defer b.Close()
	r := NewRevoker(RevokerConfig{
		ExecGatewayURLs: []string{a.URL, b.URL},
		InternalSharedSecret: "s",
		HTTP:            a.Client(),
		Logger:          discardLogger(),
		Timeout:         time.Second,
	})
	r.Revoke(context.Background(), "trn_x")
	// Revoke is synchronous (returns after fan-out completes), so values are settled.
	if atomic.LoadInt32(&hitsA) != 1 || atomic.LoadInt32(&hitsB) != 1 {
		t.Errorf("hits: a=%d b=%d", hitsA, hitsB)
	}
}

func TestRevoker_OneReplicaDown_OthersStillFire(t *testing.T) {
	var hitOK int32
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hitOK, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	bad := "http://127.0.0.1:1" // refused
	r := NewRevoker(RevokerConfig{
		ExecGatewayURLs: []string{bad, ok.URL},
		InternalSharedSecret: "s",
		HTTP:            ok.Client(),
		Logger:          discardLogger(),
		Timeout:         200 * time.Millisecond,
	})
	r.Revoke(context.Background(), "trn_y")
	if atomic.LoadInt32(&hitOK) != 1 {
		t.Errorf("ok hit: %d", hitOK)
	}
}

// silence "declared but not used" warnings for sync import
var _ = sync.Mutex{}
```

- [ ] **Step 2: Run, verify FAIL**

Run: `go test ./internal/codexappgateway/ -run TestRevoker -v`
Expected: FAIL — `NewRevoker` undefined.

- [ ] **Step 3: Implement**

`internal/codexappgateway/revoke.go`:
```go
package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type RevokerConfig struct {
	ExecGatewayURLs      []string // e.g. ["http://codex-exec-gateway-0:6060","http://...-1:6060"]
	InternalSharedSecret string   // bearer for exec-gateway internal API (CXG_INTERNAL_SHARED_SECRET)
	HTTP                 *http.Client
	Logger               *slog.Logger
	Timeout              time.Duration
}

type httpRevoker struct{ cfg RevokerConfig }

func NewRevoker(cfg RevokerConfig) Revoker {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &httpRevoker{cfg: cfg}
}

func (r *httpRevoker) Revoke(parent context.Context, turnID string) {
	body, _ := json.Marshal(map[string]string{"turn_id": turnID})
	var wg sync.WaitGroup
	for _, url := range r.cfg.ExecGatewayURLs {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parent, r.cfg.Timeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				url+"/api/exec-gateway/revoke-turn", bytes.NewReader(body))
			if err != nil {
				r.cfg.Logger.Warn("revoke build req", "url", url, "error", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+r.cfg.InternalSharedSecret)
			resp, err := r.cfg.HTTP.Do(req)
			if err != nil {
				r.cfg.Logger.Warn("revoke POST", "url", url, "turn_id", turnID, "error", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				r.cfg.Logger.Warn("revoke status", "url", url, "turn_id", turnID, "status", resp.StatusCode)
			}
		}(url)
	}
	wg.Wait()
	_ = fmt.Sprintf("noop") // keep fmt import live for future log formatting
}
```

- [ ] **Step 4: Run, verify PASS**

Run: `go test ./internal/codexappgateway/ -run TestRevoker -v`
Expected: PASS for both tests.

- [ ] **Step 5: Commit**

```bash
git add internal/codexappgateway/revoke.go internal/codexappgateway/revoke_test.go
git commit -m "feat(codex-app-gateway): revocation fan-out to all exec-gateway replicas"
```

---

## Task 11: End-to-end integration test

**Files:**
- Create: `internal/codexappgateway/e2e_test.go`

Drives one whole turn through the assembled stack (handlers + worker
registry + driver + revoker) with three doubles: a fake exec-gateway
HTTP server, a fake `Driver` that emits a scripted ThreadEvent stream,
and the in-memory store from Task 8. Asserts:

1. `initialize` → `InitializeResponse` with phase-1 caps.
2. `thread/start` → persisted thread row.
3. `turn/start` → `TurnStartResponse{status:"inProgress"}` synchronously.
4. The pumped notifications arrive on the `Broadcaster` in the expected
   order (`thread/started`, `turn/started`, `item/started`,
   `item/completed`, `turn/completed`).
5. `codex_turn_events` rows are persisted in the store after the turn
   ends.
6. The fake exec-gateway received exactly one `POST /api/exec-gateway/revoke-turn`.

- [ ] **Step 1: Write the failing test**

`internal/codexappgateway/e2e_test.go`:
```go
package codexappgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codex "github.com/agentserver/codex-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/codexappgateway/handlers"
	"github.com/agentserver/agentserver/internal/codexappgateway/protocol"
	"github.com/agentserver/agentserver/internal/codexappgateway/runner"
	"github.com/agentserver/agentserver/internal/codexappgateway/store"
)

// e2eFakeStore composes the per-handler fakes so it satisfies every
// store interface used in the runtime (ThreadStore, TurnStore, WorkerStore).
type e2eFakeStore struct {
	mu       sync.Mutex
	threads  map[string]store.Thread
	turns    map[string][]store.AgentTurn
	pending  []*store.AgentTurn
	events   map[string][]json.RawMessage
	terminal map[string]string // turnID → "done"/"failed"/"cancelled"
}

func newE2EStore() *e2eFakeStore {
	return &e2eFakeStore{
		threads: map[string]store.Thread{},
		turns:   map[string][]store.AgentTurn{},
		events:  map[string][]json.RawMessage{},
		terminal: map[string]string{},
	}
}

func (s *e2eFakeStore) CreateThread(_ context.Context, t store.Thread) error {
	s.mu.Lock(); defer s.mu.Unlock()
	s.threads[t.ID] = t; return nil
}
func (s *e2eFakeStore) GetThread(_ context.Context, id string) (store.Thread, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	t, ok := s.threads[id]
	if !ok { return store.Thread{}, errFake("not found") }
	return t, nil
}
func (s *e2eFakeStore) ListThreads(_ context.Context, _ string, _, _ int) ([]store.Thread, error) {
	return nil, nil
}
func (s *e2eFakeStore) ListTurns(_ context.Context, tid string, _, _ int) ([]store.AgentTurn, error) {
	return s.turns[tid], nil
}
func (s *e2eFakeStore) ListEvents(_ context.Context, tid string, _ int64) ([]store.TurnEvent, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	out := make([]store.TurnEvent, 0, len(s.events[tid]))
	for i, p := range s.events[tid] {
		out = append(out, store.TurnEvent{TurnID: tid, SeqNum: int64(i + 1), Payload: p})
	}
	return out, nil
}
func (s *e2eFakeStore) EnqueueTurn(_ context.Context, t store.AgentTurn) error {
	s.mu.Lock(); defer s.mu.Unlock()
	cp := t
	s.turns[t.ThreadID] = append(s.turns[t.ThreadID], cp)
	s.pending = append(s.pending, &cp)
	return nil
}
func (s *e2eFakeStore) PickNextPending(_ context.Context, threadID string) (*store.AgentTurn, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	for i, t := range s.pending {
		if t.ThreadID == threadID {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return t, nil
		}
	}
	return nil, nil
}
func (s *e2eFakeStore) MarkTurnRunning(_ context.Context, _ string) error  { return nil }
func (s *e2eFakeStore) MarkTurnDone(_ context.Context, id string) error {
	s.mu.Lock(); defer s.mu.Unlock(); s.terminal[id] = "done"; return nil
}
func (s *e2eFakeStore) MarkTurnFailed(_ context.Context, id, _ string) error {
	s.mu.Lock(); defer s.mu.Unlock(); s.terminal[id] = "failed"; return nil
}
func (s *e2eFakeStore) MarkTurnCancelled(_ context.Context, id string) error {
	s.mu.Lock(); defer s.mu.Unlock(); s.terminal[id] = "cancelled"; return nil
}
func (s *e2eFakeStore) InsertEvent(_ context.Context, tid string, p json.RawMessage) (int64, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.events[tid] = append(s.events[tid], p)
	return int64(len(s.events[tid])), nil
}

type errFakeT string
func errFake(s string) error { return errFakeT(s) }
func (e errFakeT) Error() string { return string(e) }

type e2eBroadcaster struct {
	mu sync.Mutex
	pushed []json.RawMessage
}
func (b *e2eBroadcaster) Push(_ string, p json.RawMessage) {
	b.mu.Lock(); defer b.mu.Unlock(); b.pushed = append(b.pushed, p)
}
func (b *e2eBroadcaster) snapshot() []json.RawMessage {
	b.mu.Lock(); defer b.mu.Unlock()
	out := make([]json.RawMessage, len(b.pushed))
	copy(out, b.pushed)
	return out
}

type e2eDriver struct{}

func (e2eDriver) Run(_ context.Context, _ runner.RunInput) (runner.DriverStream, error) {
	ch := make(chan codex.ThreadEvent, 8)
	ch <- &codex.ThreadStartedEvent{Type: "thread.started", ThreadID: "thr_e2e"}
	ch <- &codex.TurnStartedEvent{Type: "turn.started"}
	item := &codex.AgentMessageItem{ID: "itm_a", Type: "agent_message", Text: "hi"}
	ch <- &codex.ItemStartedEvent{Type: "item.started", Item: item}
	ch <- &codex.ItemCompletedEvent{Type: "item.completed", Item: item}
	ch <- &codex.TurnCompletedEvent{Type: "turn.completed", Usage: codex.Usage{InputTokens: 10}}
	close(ch)
	return &e2eStream{ch: ch}, nil
}

type e2eStream struct{ ch chan codex.ThreadEvent }
func (s *e2eStream) Events() <-chan codex.ThreadEvent { return s.ch }
func (s *e2eStream) Wait() error                       { return nil }

func TestE2E_OneTurnFullRoundTrip(t *testing.T) {
	store := newE2EStore()

	// Fake exec-gateway: serves /connected (returns one executor) and /revoke-turn.
	var revokes int32
	exgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/exec-gateway/connected":
			_ = json.NewEncoder(w).Encode([]runner.ConnectedExecutor{
				{ExeID: "exe_alpha", Description: "fake", IsDefault: true},
			})
		case "/api/exec-gateway/revoke-turn":
			atomic.AddInt32(&revokes, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer exgw.Close()

	// Workspace fake: just returns paths in a tmp dir.
	wsFake := &fakeWorkspace{tmpRoot: t.TempDir()}

	// Manifest writer using the fake exec-gateway.
	mw := runner.NewManifestWriter(runner.ManifestConfig{
		ExecGatewayHTTPURL:   exgw.URL,
		ExecGatewayWSURL:     "ws://example",
		InternalSharedSecret: "s",
		CapTokenHMACSecret:   []byte("k"),
		TmpRoot:              t.TempDir(),
		HTTP:                 exgw.Client(),
		Now:                  time.Now,
	})

	bcast := &e2eBroadcaster{}
	rev := NewRevoker(RevokerConfig{
		ExecGatewayURLs: []string{exgw.URL},
		InternalSharedSecret: "s", HTTP: exgw.Client(),
		Logger: discardLogger(), Timeout: time.Second,
	})
	deps := WorkerDeps{
		Store: store, Workspace: wsFake, Manifest: mw,
		Driver: e2eDriver{}, Broadcaster: bcast, Revoker: rev,
		WSToken: func(_ context.Context, _ string) (string, error) { return "wsTok", nil },
		Logger:  discardLogger(),
		TmpRoot: t.TempDir(),
	}
	reg := NewWorkerRegistry(func(threadID string) *sessionWorker {
		w := newSessionWorker(threadID, deps, nil)
		return w
	})
	deps.Registry = reg

	// 1. initialize
	initH := handlers.NewInitialize()
	st := &handlers.ConnState{UserID: "u_1", WorkspaceID: "ws_1", ConnID: "c1"}
	if _, err := initH.Handle(context.Background(), st, nil); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// 2. thread/start
	thH := handlers.NewThreadHandlers(store, nil, time.Now)
	startResp, err := thH.Start(context.Background(), st, json.RawMessage(`{"title":"e2e"}`))
	if err != nil {
		t.Fatalf("thread/start: %v", err)
	}
	threadID := startResp.(protocol.ThreadStartResponse).Thread.ID

	// 3. turn/start
	tnH := handlers.NewTurnHandlers(store, reg, time.Now)
	body, _ := json.Marshal(map[string]any{
		"threadId": threadID,
		"input":    []map[string]string{{"type": "text", "text": "hello"}},
	})
	turnResp, err := tnH.Start(context.Background(), st, body)
	if err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	turnID := turnResp.(protocol.TurnStartResponse).Turn.ID
	if turnResp.(protocol.TurnStartResponse).Turn.Status != "inProgress" {
		t.Fatalf("status: %s", turnResp.(protocol.TurnStartResponse).Turn.Status)
	}

	// 4. wait for the worker to finish (deterministic on fake driver).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		state := store.terminal[turnID]
		store.mu.Unlock()
		if state == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := store.terminal[turnID]; got != "done" {
		t.Fatalf("turn never reached done; state=%q", got)
	}

	// 5. assert pushed notifications in expected order. Sequence is:
	//   thread/status/changed=running   (publishStatus, worker takeover)
	//   thread/started, turn/started, item/started, item/completed,
	//   turn/completed                   (mapper, from codex events)
	//   turn/completed                   (publishTerminal, worker exit)
	//   thread/status/changed=idle       (publishStatus, mirror terminal)
	// The duplicate `turn/completed` is deliberate: one from codex's own
	// TurnCompletedEvent (mapper), one synthetic from the worker's
	// publishTerminal so a TUI's "wait for end" select unblocks even if
	// codex never emits a terminal event (e.g. crash mid-stream).
	pushed := bcast.snapshot()
	wantMethods := []string{
		"thread/status/changed",
		"thread/started", "turn/started", "item/started", "item/completed",
		"turn/completed",
		"turn/completed",
		"thread/status/changed",
	}
	if len(pushed) != len(wantMethods) {
		t.Fatalf("pushed: got %d want %d (%v)", len(pushed), len(wantMethods), pushed)
	}
	for i, want := range wantMethods {
		var n struct{ Method string `json:"method"` }
		_ = json.Unmarshal(pushed[i], &n)
		if n.Method != want {
			t.Errorf("notif[%d]: got %q want %q", i, n.Method, want)
		}
	}

	// 6. assert events persisted (excludes the worker-emitted terminal turn/done envelope).
	if got := len(store.events[turnID]); got != 5 {
		t.Errorf("persisted events: got %d want 5", got)
	}

	// 7. assert revocation fired exactly once
	if r := atomic.LoadInt32(&revokes); r != 1 {
		t.Errorf("revocations: got %d want 1", r)
	}
}
```

- [ ] **Step 2: Run, verify FAIL on first attempt**

Run: `go test ./internal/codexappgateway/ -run TestE2E -v`
Expected: FAIL initially while you reconcile any small drift between
the handlers' request-shape parsing and the test's hand-written
JSON. Fix one delta at a time until PASS — do **not** invent new
handler methods or store fields; either the test or 2a's existing
types are wrong.

- [ ] **Step 3: Run, verify PASS**

Run: `go test ./internal/codexappgateway/... -v`
Expected: all tests PASS in the codexappgateway tree.

- [ ] **Step 4: Commit**

```bash
git add internal/codexappgateway/e2e_test.go
git commit -m "test(codex-app-gateway): e2e one-turn round-trip (fake driver + fake exec-gateway)"
```

---

## Self-Review

### 1. Spec coverage

| Spec section / requirement | Plan task |
|---|---|
| 8 ClientRequests + 1 ClientNotification (phase 1 list) | Tasks 1–4 (`initialize`, all `thread/*`, `turn/start`, `turn/interrupt`); Plan 2a owns the `initialized` notification path |
| 8 ServerNotifications mapping table | Task 7 covers all 8 (`thread/started`, `thread/status/changed` falls under `error`/`turn/*` lifecycle in 2a's status fan-out, `turn/started`, `turn/completed`, `item/started`, `item/completed`, `item/agentMessage/delta`, `error`). **Note:** `thread/status/changed` is the one phase-1 notification with no codex source event — it's emitted by 2a on `EnqueueTurn`/`MarkTurn*`. Flagged for 2a audit |
| Turn lifecycle steps 1–8 | Task 8 |
| Manifest construction (live probe + token mint + 0600 file) | Task 5 |
| `CODEX_EXEC_GATEWAY_TOKEN` payload exe_ids | Task 5 (asserted in test) |
| Workspace persistence (S3 download, tmp dirs, env injection) | 2a owns Setup/Teardown; Task 6 injects env; Task 8 wires the lifecycle |
| `ApprovalPolicy=never` invariant | Task 6 default + comment |
| Reconnection: `thread/read` replays events | Task 2 |
| Reconnection: live event push to new connection | 2a's `Broadcaster` is keyed by `thread_id`, so a new conn that subscribes mid-turn receives subsequent pushes — assumed, not tested here |
| Recovery: ResetRunningToQueued + notify | Task 9 |
| Revocation push fan-out | Task 10 |
| Per-turn cleanup defer-driven | Task 8 (manifest cleanup, workspace teardown, revoke all `defer`-bound) |

### 2. Placeholder scan

No "TBD" / "implement later" / "see ccbroker without showing code" /
"add error handling" found. Every step has a runnable command +
expected output, every code block compiles against the imports it
declares. The phrase "var _ = sync.Mutex{}" / "var _ = fmt.Sprintf" in
two test/source files is a deliberate keep-import-live device, not a
placeholder.

### 3. Type consistency with Plan 2a (assumed)

The plan references the following 2a-owned identifiers without
defining them:

- `protocol.ClientRequest`, `protocol.ServerNotification`,
  `protocol.InitializeResponse`, `protocol.ServerInfo`,
  `protocol.Capabilities`, `protocol.Thread`, `protocol.Turn`,
  `protocol.PersistedEvent`, `protocol.ThreadStartResponse`,
  `protocol.ThreadResumeResponse{Thread, Diagnostic}`,
  `protocol.ThreadReadResponse{Thread, Turns, Events}`,
  `protocol.ThreadListResponse{Threads}`,
  `protocol.TurnListResponse{Turns}`,
  `protocol.TurnStartResponse{Turn}`,
  `protocol.TurnInterruptResponse{Cancelled}`.
- `store.Thread{ID, WorkspaceID, UserID, Title, Status, CreatedAt,
  UpdatedAt, Metadata json.RawMessage}`.
- `store.AgentTurn{ID, ThreadID, WorkspaceID, UserInput
  json.RawMessage, TurnOptions json.RawMessage, Status, EnqueuedAt,
  StartedAt, FinishedAt}`.
- `store.TurnEvent{TurnID, SeqNum int64, Payload json.RawMessage}`.
- `exectoken.Mint(MintInput) (string, error)`,
  `exectoken.Verify(secret []byte, token string) (Claims, error)`,
  `exectoken.MintInput{Secret []byte, TurnID, WorkspaceID string,
  ExeIDs []string, TTL time.Duration, Now time.Time}`,
  `exectoken.Claims{TurnID, WorkspaceID string, ExeIDs []string,
  IssuedAt, ExpiresAt int64}`.

If 2a names any of these differently, search-and-replace in this
plan's import / type-assertion sites. No structural changes needed.

### 4. Any spec ambiguity resolved

- Spec says `item/agentMessage/delta` "comes from synthesizing on
  item.updated where applicable" but does not specify how to compute
  the delta from a full-text update. **Resolved** in Task 7: keep a
  per-item `agentMsgSeen` map; if the new text starts with the old
  text, emit the suffix; otherwise emit the full new text and reset.
  This matches every observed codex behavior (text always grows
  monotonically per `item_id`); a regression to non-monotonic text
  degrades gracefully to "emit full text as the delta," which the TUI
  can still render correctly because it tracks per-item state too.
- Spec is silent on whether `turn/done` / `turn/cancelled` /
  `turn/failed` are themselves ServerNotifications (the 8-method table
  lists `turn/completed` as the only positive terminal). **Resolved**
  in Task 8: the worker emits a typed `turn/completed` (or `error` for
  failures) on every terminal exit via the same sum-type that the mapper
  uses, so a connected TUI's "wait for turn end" select unblocks even
  if codex never emits a TurnCompletedEvent. These envelopes are pushed
  but not persisted (they reflect connection-visible state, not
  codex-emitted history). No bespoke `turn/done` / `turn/cancelled`
  methods are introduced — both terminal kinds carry through the
  existing `turn/completed` notification with the appropriate
  `Turn.Status`.
