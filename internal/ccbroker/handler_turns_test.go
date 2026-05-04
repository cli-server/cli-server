package ccbroker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ----- fakeStore: in-memory implementation of storer -----

type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	events   []EventInput

	// Recorded inserted events so GetTurnEvents can serve catch-up tests.
	recordedEvents []fakeEvent
	nextSeqNum     int64

	// Queue state
	turns      map[string]*AgentTurn // by turn_id
	turnOrder  map[string][]string   // session_id → ordered turn_ids
	resetCount int
}

type fakeEvent struct {
	SeqNum    int64
	TurnID    string
	EventID   string
	EventType string
	Payload   json.RawMessage
	CreatedAt time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions:  make(map[string]*Session),
		turns:     make(map[string]*AgentTurn),
		turnOrder: make(map[string][]string),
	}
}

func (f *fakeStore) GetSession(_ context.Context, id string) (*Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id], nil
}

func (f *fakeStore) CreateSession(_ context.Context, id, workspaceID, title, source string, externalID *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id] = &Session{ID: id, WorkspaceID: workspaceID, Title: title, Source: source, ExternalID: externalID}
	return nil
}

func (f *fakeStore) GetSessionEpoch(_ context.Context, sessionID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[sessionID]; ok {
		return s.Epoch, nil
	}
	return 0, nil
}

func (f *fakeStore) InsertEvents(ctx context.Context, sessionID string, epoch int, events []EventInput) ([]InsertedEvent, error) {
	return f.InsertEventsWithTurn(ctx, sessionID, epoch, "", events)
}

func (f *fakeStore) InsertEventsWithTurn(_ context.Context, _ string, _ int, turnID string, events []EventInput) ([]InsertedEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, events...)
	out := make([]InsertedEvent, 0, len(events))
	for _, e := range events {
		f.nextSeqNum++
		seq := f.nextSeqNum
		// Derive a synthetic event_type from the payload so tests can drive
		// catch-up scenarios with terminal events. Production hardcodes
		// 'client_event'; we mimic that as the default.
		evtType := "client_event"
		if len(e.Payload) > 0 {
			var probe struct {
				EventType string `json:"event_type"`
			}
			if json.Unmarshal(e.Payload, &probe) == nil && probe.EventType != "" {
				evtType = probe.EventType
			}
		}
		f.recordedEvents = append(f.recordedEvents, fakeEvent{
			SeqNum:    seq,
			TurnID:    turnID,
			EventID:   e.EventID,
			EventType: evtType,
			Payload:   append(json.RawMessage(nil), e.Payload...),
			CreatedAt: time.Now(),
		})
		out = append(out, InsertedEvent{SeqNum: seq, EventID: e.EventID})
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
	if limit <= 0 || limit > 200 {
		limit = 50
	}
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

func (f *fakeStore) GetTurnEvents(_ context.Context, turnID string, sinceSeqNum int64) ([]TurnEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []TurnEvent
	for _, e := range f.recordedEvents {
		if e.TurnID != turnID || e.SeqNum <= sinceSeqNum {
			continue
		}
		out = append(out, TurnEvent{
			SeqNum:    e.SeqNum,
			EventID:   e.EventID,
			EventType: e.EventType,
			Payload:   e.Payload,
			CreatedAt: e.CreatedAt,
		})
	}
	return out, nil
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

// ----- Task 8 tests -----

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

// TestHandleProcessTurn_RespectsCallerTurnID asserts that when the request
// body includes a non-empty turn_id, cc-broker uses it as agent_turns.id
// instead of generating a new one. Used by the TUI inbound caller which
// pre-CASes its active_turn_id agentserver-side.
func TestHandleProcessTurn_RespectsCallerTurnID(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	registry := newWorkerRegistry(workerDeps{
		store: store, sse: sse,
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	registry.executeOverride = func(_ context.Context, turn *AgentTurn) {
		_ = store.MarkTurnDone(context.Background(), turn.ID)
		sse.Publish(turn.SessionID, &StreamClientEvent{
			EventID:   "evt_t",
			EventType: "turn_done",
			TurnID:    turn.ID,
			Payload:   json.RawMessage(`{}`),
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		})
	}
	defer registry.Shutdown(context.Background())

	srv := &Server{
		store: store, sse: sse,
		activeTurns:    newActiveTurnRegistry(),
		compactQueue:   newCompactQueue(),
		workerRegistry: registry,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	body := bytes.NewBufferString(`{"session_id":"sess_c","workspace_id":"ws","user_message":"hi","turn_id":"trn_caller_supplied"}`)
	req := httptest.NewRequest("POST", "/api/turns", body)
	rec := httptest.NewRecorder()
	srv.handleProcessTurn(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	turns, _ := store.ListSessionTurns(context.Background(), "sess_c", 10)
	if len(turns) != 1 || turns[0].ID != "trn_caller_supplied" {
		t.Fatalf("expected caller-supplied id, got %+v", turns)
	}
	if !strings.Contains(rec.Body.String(), `"turn_id":"trn_caller_supplied"`) {
		t.Fatalf("expected turn_started prelude to echo caller turn_id, got %s", rec.Body.String())
	}
}
