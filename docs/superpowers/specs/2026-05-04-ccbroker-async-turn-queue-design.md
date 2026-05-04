# ccbroker: async per-session turn queue

**Date:** 2026-05-04
**Status:** design
**Scope:** ccbroker turn-execution model
**Related:** issue #68 (`turnLock` blocks HTTP indefinitely; in-flight cancellation surface is incomplete), issue #69 (concurrent same-workspace memory writes)
**Reference:** `/root/cc/source/src/utils/messageQueueManager.ts` — Claude Code's own queue model

## Goal

Replace cc-broker's `turnLock`-based blocking model with a **producer/worker decoupled** async queue:

- **Producer** (HTTP `POST /api/turns`) does only light work: validate, persist user-message event, persist turn record, signal worker. Then either (a) returns 202 immediately (new v2 API) or (b) subscribes to SSE and streams events for this turn (existing API, backward-compat wrapper).
- **Worker** (per-session goroutine) does the heavy work: workspace Setup → wstoken acquire → runner.Run → broadcast events to SSE + persist to DB → workspace Teardown → mark turn done.
- **Crash recovery**: on cc-broker restart, scan DB for unfinished turns (`state IN ('queued','running')`) and re-enqueue.
- **Cancel** reaches both queued (remove from queue) and running (cancel runner ctx) turns.

## Non-goals

- **Priority levels** (CC has now/next/later). IM scenario doesn't need them; FIFO per session is sufficient.
- **Interrupt-on-new-message**. CC interrupts only when the running turn is in an explicitly-interruptible tool. We have no such tool concept; new messages just queue.
- **Cross-replica distributed queue**. cc-broker is single-replica by design (see `workspace.go:46`); in-process state is fine.
- **Automatic turn retry on failure**. Failed turns are marked `failed`; the user / client decides whether to retry by sending a new turn.

## Motivation (recap of the problem)

`internal/ccbroker/turn_lock.go` is a per-sessionID buffered channel. `Acquire(sid)` blocks until release. Concrete failures observed:

1. **Goroutine leak on client disconnect**: client sends POST, cc-broker is busy with a prior turn. Lock blocks the new handler. Client times out and disconnects. Handler's `r.Context()` cancels, but `Acquire` doesn't observe context — handler keeps blocking until the prior turn finishes, then runs the full LLM + S3 cycle for a request nobody's listening to.

2. **`activeTurns.Set` overwrite bug**: T2 handler calls `activeTurns.Set(sid, T2, cancelT2)` *before* `turnLock.Acquire`. If T1 is still running, T2's entry overwrites T1's. `POST /cancel/T1` then no-ops because `activeTurns.Cancel` only matches by current TurnID.

3. **No queue depth bound**: a burst of N messages from one session creates N blocked handler goroutines, each holding HTTP connection state.

4. **No crash safety**: a turn in-flight when cc-broker pod restarts is silently lost. The user's IM message gets no reply.

CC's own model addresses (1) and (4) by separating producer (UI submit) from consumer (a single processing loop). We follow the same shape.

## Architecture

```
HTTP POST /api/turns
   ↓ (validate)
   ├─ INSERT INTO agent_events (user-message, turn_id)
   ├─ INSERT INTO agent_turns  (state='queued')
   └─ workerRegistry.Notify(sessionID)
                                     ↓
                      ┌─────────────────────────────┐
                      │ per-session sessionWorker   │
                      │ goroutine (sleeps when idle)│
                      │  for {                      │
                      │    t := store.PickNext(sid) │
                      │    if t == nil:             │
                      │      wait wake|idle|quit    │
                      │    execute(t)               │
                      │  }                          │
                      └─────────────────────────────┘
                                     ↓
                  events → SSEBroker.Publish(sid, event)
                        → INSERT INTO agent_events (turn_id, payload)
                        → store.MarkRunning / MarkDone / MarkFailed

HTTP subscribers (any number, any time):
  GET /api/sessions/{sid}/events?since=N   ← session-wide, all turns
  GET /api/turns/{tid}/events?since=N       ← single turn, until terminal event
```

## Components

### `internal/db/migrations/022_agent_turns.sql` (new)

```sql
CREATE TABLE agent_turns (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    workspace_id  TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('queued','running','done','cancelled','failed')),
    user_event_id TEXT NOT NULL,
    metadata      JSONB NOT NULL DEFAULT '{}',
    im_channel_id TEXT,
    im_user_id    TEXT,
    error_msg     TEXT,
    enqueued_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ
);
CREATE INDEX idx_agent_turns_pending ON agent_turns(session_id, enqueued_at)
    WHERE state IN ('queued','running');
CREATE INDEX idx_agent_turns_session ON agent_turns(session_id, enqueued_at DESC);

ALTER TABLE agent_events ADD COLUMN turn_id TEXT;
CREATE INDEX idx_agent_events_turn ON agent_events(turn_id) WHERE turn_id IS NOT NULL;
```

### `internal/db/agent_turns.go` (new)

`turnStore` SQL wrapper:

```go
type AgentTurn struct {
    ID, SessionID, WorkspaceID string
    State                       string  // queued|running|done|cancelled|failed
    UserEventID                 string
    Metadata                    json.RawMessage
    IMChannelID, IMUserID       sql.NullString
    ErrorMsg                    sql.NullString
    EnqueuedAt                  time.Time
    StartedAt, FinishedAt       sql.NullTime
}

func (db *DB) EnqueueTurn(ctx context.Context, t AgentTurn) error
func (db *DB) PickNextPending(ctx context.Context, sessionID string) (*AgentTurn, error)
func (db *DB) MarkTurnRunning(ctx context.Context, turnID string) error
func (db *DB) MarkTurnDone(ctx context.Context, turnID string) error
func (db *DB) MarkTurnCancelled(ctx context.Context, turnID string) error
func (db *DB) MarkTurnFailed(ctx context.Context, turnID, errMsg string) error
func (db *DB) ListSessionsWithPending(ctx context.Context) ([]string, error)  // recovery
func (db *DB) ListQueuedTurns(ctx context.Context, sessionID string) ([]string, error)
func (db *DB) GetTurn(ctx context.Context, turnID string) (*AgentTurn, error)
func (db *DB) ResetRunningToQueued(ctx context.Context) (int, error)  // recovery
func (db *DB) CountPending(ctx context.Context, sessionID string) (int, error)  // depth check
```

`PickNextPending` is `SELECT ... ORDER BY enqueued_at LIMIT 1 WHERE session_id=$1 AND state IN ('queued','running')`. We include `running` in case a worker crashed mid-execute and recovery rebooted us — it'll pick that turn back up. `MarkRunning` is `UPDATE state='running', started_at=NOW() WHERE id=$1 AND state='queued'`; rejecting if not still queued is fine because PickNextPending already filtered.

### `internal/ccbroker/session_worker.go` (new)

```go
const IdleTimeout = 5 * time.Minute

type sessionWorker struct {
    sessionID string
    wake      chan struct{}  // buffer=1
    quit      chan struct{}
    deps      workerDeps
    onIdleExit func(sessionID string)  // workerRegistry callback
}

type workerDeps struct {
    store            *Store
    s3               *workspace.S3Store
    wstoken          func(ctx context.Context, wid string) (string, error)
    sse              *SSEBroker
    activeTurns      *activeTurnRegistry
    compactQueue     *compactQueue
    callTurnFinished func(sessionID, turnID string)
    config           Config
    logger           *slog.Logger
    gate             *tools.Gate
    runnerRun        func(...) (<-chan agentsdk.SDKMessage, error)
    workspaceSetup   func(...) (*workspace.Workspace, error)
    workspaceTeardown func(...) error
}

func (w *sessionWorker) run(ctx context.Context) {
    idle := time.NewTimer(IdleTimeout)
    for {
        turn, err := w.deps.store.PickNextPending(ctx, w.sessionID)
        if err != nil {
            w.deps.logger.Error("worker pick next failed", "error", err)
            // sleep + retry; don't tight-loop
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
            w.execute(ctx, turn)
            if !idle.Stop() { <-idle.C }
            idle.Reset(IdleTimeout)
            continue
        }
        select {
        case <-w.wake:
            if !idle.Stop() { <-idle.C }
            idle.Reset(IdleTimeout)
        case <-idle.C:
            w.onIdleExit(w.sessionID)
            return
        case <-w.quit:
            return
        case <-ctx.Done():
            return
        }
    }
}
```

`execute(ctx, turn)` is the heavy path. It mirrors the current `handleProcessTurn` work between the lock-acquire and the SSE-pump goroutine, but bound to worker context (not HTTP):

```go
func (w *sessionWorker) execute(ctx context.Context, turn *AgentTurn) {
    turnCtx, cancel := context.WithCancel(ctx)
    w.deps.activeTurns.Set(turn.SessionID, turn.ID, cancel)
    defer w.deps.activeTurns.Clear(turn.SessionID, turn.ID)
    defer cancel()
    defer w.deps.callTurnFinished(turn.SessionID, turn.ID)

    if err := w.deps.store.MarkTurnRunning(ctx, turn.ID); err != nil {
        w.deps.logger.Error("mark running failed", "turn_id", turn.ID, "error", err)
        return  // worker keeps going; turn stays in 'queued', will be retried next loop
    }

    // wstoken
    wsTok, err := w.deps.wstoken(ctx, turn.WorkspaceID)
    if err != nil {
        w.failTurn(ctx, turn, "workspace token: " + err.Error())
        return
    }

    // workspace Setup
    ws, err := w.deps.workspaceSetup(ctx, turn.WorkspaceID, turn.SessionID, w.deps.s3)
    if err != nil {
        w.failTurn(ctx, turn, "workspace setup: " + err.Error())
        return
    }
    defer w.deps.workspaceTeardown(context.Background(), ws, w.deps.s3)

    // Build tools.Context, runCfg (same fields as current handler) ...
    // ...

    // Pump runner output → DB + SSE, tagged with turn_id
    msgCh, err := w.deps.runnerRun(turnCtx, ws, turn.SessionID, userMessage, runCfg, mcp)
    if err != nil {
        w.failTurn(ctx, turn, "runner.Run: " + err.Error())
        return
    }
    epoch, _ := w.deps.store.GetSessionEpoch(ctx, turn.SessionID)
    for sdkMsg := range msgCh {
        evt, _ := runner.ToEventPayload(sdkMsg)
        eventID := uuid.NewString()
        var seqNum int64
        if !evt.Ephemeral {
            inserted, _ := w.deps.store.InsertEventsWithTurn(
                ctx, turn.SessionID, epoch, turn.ID,
                []EventInput{{EventID: eventID, Payload: evt.Payload, Ephemeral: false}},
            )
            if len(inserted) > 0 { seqNum = inserted[0].SeqNum }
        }
        w.deps.sse.Publish(turn.SessionID, &StreamClientEvent{
            EventID: eventID, SequenceNum: seqNum, EventType: evt.Type,
            TurnID: turn.ID, Payload: evt.Payload,
            CreatedAt: time.Now().Format(time.RFC3339Nano),
        })
    }

    // Final state — distinguish ctx cancel vs natural completion
    if turnCtx.Err() != nil && errors.Is(turnCtx.Err(), context.Canceled) {
        w.deps.store.MarkTurnCancelled(ctx, turn.ID)
    } else {
        w.deps.store.MarkTurnDone(ctx, turn.ID)
    }
}
```

`failTurn` marks state=failed, broadcasts a synthetic error event, and returns.

### `internal/ccbroker/worker_registry.go` (new)

```go
type workerRegistry struct {
    mu      sync.Mutex
    workers map[string]*sessionWorker
    deps    workerDeps
    ctx     context.Context  // server context; cancels on shutdown
}

func (r *workerRegistry) Notify(sessionID string) {
    r.mu.Lock()
    w, ok := r.workers[sessionID]
    if !ok {
        w = newSessionWorker(sessionID, r.deps, r.onIdleExit)
        r.workers[sessionID] = w
        go w.run(r.ctx)
    }
    r.mu.Unlock()
    select { case w.wake <- struct{}{}: default: }  // signal, drop if already pending
}

func (r *workerRegistry) onIdleExit(sessionID string) {
    r.mu.Lock()
    delete(r.workers, sessionID)
    r.mu.Unlock()
}

func (r *workerRegistry) Shutdown(ctx context.Context) error {
    r.mu.Lock()
    workers := make([]*sessionWorker, 0, len(r.workers))
    for _, w := range r.workers { workers = append(workers, w) }
    r.mu.Unlock()
    for _, w := range workers { close(w.quit) }
    // best-effort wait — workers are bounded by their current turn duration
    return nil
}
```

`Notify` is the producer's only contract. Idempotent. Non-blocking. Spawns a worker if none exists, signals an existing one if it's idle.

### `internal/ccbroker/recovery.go` (new)

```go
func (s *Server) recoverPendingTurns(ctx context.Context) error {
    n, err := s.store.ResetRunningToQueued(ctx)
    if err != nil { return fmt.Errorf("reset running: %w", err) }
    if n > 0 {
        s.logger.Info("recovery: reset stale running turns", "count", n)
    }
    sids, err := s.store.ListSessionsWithPending(ctx)
    if err != nil { return fmt.Errorf("list pending: %w", err) }
    for _, sid := range sids {
        s.workerRegistry.Notify(sid)
    }
    s.logger.Info("recovery: notified workers", "session_count", len(sids))
    return nil
}
```

Called from Server startup before serving HTTP. A turn that was in-flight at the time of crash gets re-enqueued and runs from scratch. Side effects:
- `agent_events` already has the user-message event (idempotent on retry — same eventID would be skipped on insert; but we can also tolerate duplicate non-user events from the prior partial run since we use uuid eventIDs)
- `claude-home.tar.gz` may have been partially uploaded → next Setup downloads whatever the last successful Teardown left
- Subscribers to the new run see all events; old subscribers (from the prior run) are gone with the crashed pod

Accepted: under crash, callers may see partial duplicate event streams. Sequence numbers come from agent_events.seq_num, monotonic, so idempotent dedupe at the client is straightforward.

### `internal/ccbroker/handler_turns.go` (modified — sync wrapper)

The **existing endpoint stays backward-compatible**. imbridge keeps working unchanged. The handler:

1. Validates input + generates `turn_id`
2. Ensures session exists, fetches epoch
3. Persists user message into `agent_events` with `turn_id`
4. Inserts `agent_turns` row with `state='queued'`
5. Calls `s.workerRegistry.Notify(sid)`
6. Subscribes to SSEBroker for this `sid`, filters by `turn_id`
7. Streams events to HTTP until a terminal event (`result` or `cancelled` or `failed`) for this `turn_id` arrives, OR client disconnects
8. Returns

Depth check **before step 4**: `if s.store.CountPending(sid) >= 16: return 429`.

The handler **never holds a turn lock**. Multiple concurrent POSTs for the same session each sit in their own SSE stream; the worker processes them serially via DB-backed queue.

`activeTurns.Set` is no longer called from this file — moved into `sessionWorker.execute`. This fixes the overwrite bug (issue #68 §2).

### `internal/ccbroker/handler_turns_v2.go` (new — async API)

```
POST /api/v2/turns
  body: { session_id, workspace_id, user_message, im_channel_id?, im_user_id?, metadata? }
  resp: 202 Accepted
        { "turn_id": "trn_...", "events_url": "/api/turns/trn_xxx/events" }
        OR 429 Too Many Requests if per-session pending >= 16
```

Same logic as steps 1-5 of v1 wrapper, then `WriteHeader(202)` + JSON body. No SSE stream.

### `internal/ccbroker/handler_turn_events.go` (new)

```
GET /api/turns/{tid}/events?since=<seq_num>
  → SSE stream
  → catch-up from agent_events WHERE turn_id=$1 AND seq_num > since
  → then live-tail SSEBroker filtered by turn_id
  → close on terminal event for this turn (result | cancelled | failed)
```

Catch-up + tail is the standard pattern. Race window between "finished catchup query" and "started subscribing" is closed by overlapping: subscribe first, then query, then de-duplicate by seq_num as we transition from catchup to live.

### `internal/ccbroker/handler_session_turns.go` (new — observability)

```
GET /api/sessions/{sid}/turns
  resp: { "turns": [
    { "turn_id", "state", "enqueued_at", "started_at", "finished_at", "error_msg" },
    ...
  ] }
```

Simple SELECT. Used by TUI / debug.

### `internal/ccbroker/handler_tui_routes.go` (modified — cancel)

```go
func (s *Server) handleCancelTurn(w, r) {
    sid, tid := chi.URLParam(...)
    turn, err := s.store.GetTurn(r.Context(), tid)
    if err != nil { /* 500 */ }
    if turn == nil { /* 404 */ }
    if turn.SessionID != sid { /* 404 */ }
    switch turn.State {
    case "queued":
        s.store.MarkTurnCancelled(r.Context(), tid)
        s.sse.Publish(sid, &StreamClientEvent{
            EventType: "turn_cancelled", TurnID: tid, ...,
        })
        writeJSON(w, 200, ...)
    case "running":
        s.activeTurns.Cancel(sid, tid)  // existing path; runner ctx fires
        writeJSON(w, 200, ...)
    default:
        writeJSON(w, 410, ...)  // already terminal
    }
}
```

### `internal/ccbroker/server.go` (modified)

- Remove `turnLock` field, add `workerRegistry`
- `NewServer`: build `workerDeps`, construct `workerRegistry`
- New `Start(ctx)` method (or extend existing) that calls `recoverPendingTurns` before returning
- New `Shutdown(ctx)` calls `workerRegistry.Shutdown(ctx)`
- Routes: add `POST /api/v2/turns`, `GET /api/turns/{tid}/events`, `GET /api/sessions/{sid}/turns`

### `internal/ccbroker/sse.go` and `models.go` (modified)

- `StreamClientEvent` gains `TurnID string` field
- All publish sites populate it

## Concurrency / cancel matrix

| State at cancel time | What happens |
|---|---|
| Queued (in DB, not yet picked) | `MarkTurnCancelled`. Worker's next `PickNextPending` skips it (state != queued). Synthetic SSE event broadcast. Any HTTP handler streaming for this turn returns. |
| Running (worker has it) | `activeTurns.Cancel(sid, tid)` fires the `turnCtx.Cancel()` set up in `execute`. Runner subprocess gets ctx done → exits. `execute` notices `turnCtx.Err() == Canceled` and `MarkTurnCancelled`. Subscribers see the SDK's terminal event (or our synthetic one). |
| Done / failed / cancelled | 410 Gone. |

`activeTurns` stays as the Server-level registry. Worker registers when it picks a turn; clears when execute returns. This keeps the existing TUI cancel API working unchanged for running turns.

## HTTP API summary

| Route | Method | Status | Purpose |
|---|---|---|---|
| `/api/turns` | POST | 200 SSE | **Existing**; sync wrapper around enqueue + stream-until-terminal |
| `/api/v2/turns` | POST | 202 + JSON | **New**; pure enqueue, returns immediately |
| `/api/turns/{tid}/events` | GET | 200 SSE | **New**; per-turn event stream with catch-up + tail |
| `/api/sessions/{sid}/turns/{tid}/cancel` | POST | 200/410 | **Modified**; works for queued AND running |
| `/api/sessions/{sid}/turns` | GET | 200 JSON | **New**; list session's recent turns + states |
| `/api/sessions/{sid}/events` | GET | 200 SSE | **Existing**; session-wide tail (used by TUI) |

## Test plan

| Test file | Coverage |
|---|---|
| `internal/db/agent_turns_test.go` | Enqueue / Pick / state transitions / depth count / list pending. Skip if no pg available — follow project convention (no DB unit tests today). |
| `internal/ccbroker/session_worker_test.go` | Single turn end-to-end with stubbed runner / store / sse / wstoken. Multiple turns process in order. Idle timeout exits worker. Wake notification interrupts idle. ctx Done in flight → MarkCancelled. Runner error → MarkFailed. |
| `internal/ccbroker/worker_registry_test.go` | Notify spawns new worker. Notify on existing worker just signals. onIdleExit unregisters. Shutdown closes all quit chans. |
| `internal/ccbroker/recovery_test.go` | Reset running → queued. List sessions with pending → notify each. With fake store + counts. |
| `internal/ccbroker/handler_turns_test.go` (modified) | New cases: client disconnect mid-stream doesn't kill worker. Two POSTs same sid both stream. 429 on overflow. |
| `internal/ccbroker/handler_turn_events_test.go` (new) | catch-up + tail. since=N skips ≤N. terminal event closes stream. unknown turn → 404. |

The existing `TestProcessTurn_*` tests need updating to inject a working `workerRegistry` instead of `turnLock`. Worker can be stubbed by injecting fake deps that just synthesize a result message immediately.

## Migration plan — two PRs

### PR 1: async infrastructure + sync wrapper

Everything above except `/api/v2/turns`. imbridge keeps working unchanged; behavior is "same client experience, but internally async + crash-safe + no goroutine leak".

### PR 2: async API + imbridge migration (later, optional)

Adds `POST /api/v2/turns`. Migrates imbridge to use POST + GET events two-step. Adds metrics: queue depth gauge, turn-latency histogram, running-turn count.

PR 1 alone fixes issue #68 P1 and the crash-loss case. PR 2 unlocks "fire and forget" for clients that don't need to block.

## Risks & open questions

1. **Recovery duplicate events**: a turn that was running at crash time gets re-executed. Subscribers connected after restart see the new run only. Subscribers via DB-backed catch-up see both attempts' events for that turn_id. Mitigation: clients dedupe by event_id (uuid) or seq_num. Document this contract.

2. **Long-running turn during cc-broker restart**: turn state went `queued → running → (crash) → reset to queued → running again`. If the prior run already completed an LLM round-trip but cc-broker died before MarkDone, we re-do the LLM call. Cost: one wasted Anthropic request. Acceptable.

3. **`agent_events.turn_id` backfill**: existing rows have NULL. Queries that filter by turn_id will exclude them (correct — they belong to pre-async turns where turn_id wasn't tracked). No backfill needed.

4. **`activeTurns` is now set by worker, not handler**. Cancel API queries it for "running" state. This is a load-bearing assumption: worker MUST register before the runner actually starts, and unregister after. Tests must cover both.

5. **Worker idle timeout = 5 min** — chosen empirically. Too short = thrash spawning workers per IM message in chatty sessions. Too long = many idle goroutines. Make env-tunable, default 5m.

6. **Backpressure on SSEBroker**: each per-turn HTTP handler keeps a subscriber. If clients are slow to read and the channel fills (256 buffered), `Publish` closes the subscriber. Existing behavior; fine.

7. **`store.InsertEventsWithTurn`** is a new wrapper around the existing InsertEvents; the only difference is the additional `turn_id` parameter passed through to the SQL. Existing call sites (e.g. user-message insert in handler) also need to pass the turn_id.

## What we explicitly are NOT changing

- The `runner` package — unchanged
- The `workspace` package — unchanged
- The `tools` package — unchanged
- The `wstoken` package — unchanged
- The Helm chart — no env changes; **no chart bump needed**
- imbridge — unchanged in PR 1
