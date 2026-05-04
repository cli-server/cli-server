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

func TestTurnEventsCatchUpReplaysPastEvents(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	store.sessions["sess_c"] = &Session{ID: "sess_c", WorkspaceID: "ws"}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_c", SessionID: "sess_c", WorkspaceID: "ws", UserEventID: "u", UserMessage: "x",
	})

	// Pre-populate events tagged with the turn id. Second event embeds a
	// terminal event_type so the handler hits the catch-up early-return path
	// without needing a live tail.
	payloadA, _ := json.Marshal(map[string]string{"event_type": "client_event", "text": "hello"})
	payloadB, _ := json.Marshal(map[string]string{"event_type": "turn_done", "turn_id": "trn_c"})
	if _, err := store.InsertEventsWithTurn(context.Background(), "sess_c", 0, "trn_c", []EventInput{
		{EventID: "evt_a", Payload: payloadA},
		{EventID: "evt_b", Payload: payloadB},
	}); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	// Mark turn done so the handler can also terminate via the
	// already-terminal short-circuit if catch-up doesn't.
	_ = store.MarkTurnDone(context.Background(), "trn_c")

	srv := &Server{
		store: store, sse: sse,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r := chi.NewRouter()
	r.Get("/api/turns/{tid}/events", srv.handleTurnEvents)

	req := httptest.NewRequest("GET", "/api/turns/trn_c/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { r.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not return after catch-up terminal event")
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"event_id":"evt_a"`) {
		t.Fatalf("expected evt_a in catch-up stream, got %s", out)
	}
	if !strings.Contains(out, `"event_id":"evt_b"`) {
		t.Fatalf("expected evt_b in catch-up stream, got %s", out)
	}
	if !strings.Contains(out, `"turn_id":"trn_c"`) {
		t.Fatalf("expected turn_id tag in events, got %s", out)
	}
	if !strings.Contains(out, `"source":"catchup"`) {
		t.Fatalf("expected catchup source in events, got %s", out)
	}
	if !strings.Contains(out, `"event_type":"turn_done"`) {
		t.Fatalf("expected terminal turn_done in stream, got %s", out)
	}
}

// TestTurnEventsTerminalTurnWithPastEventsExits guards against the regression
// where a finished turn with persisted past events would be replayed and then
// hang in the live tail until keepalive/disconnect — because the worker's
// terminal events are broadcast-only (never persisted) and persisted events
// are all 'client_event'. Both past events plus the done sentinel must arrive
// promptly and the handler must exit.
func TestTurnEventsTerminalTurnWithPastEventsExits(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	store.sessions["sess_t"] = &Session{ID: "sess_t", WorkspaceID: "ws"}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_t", SessionID: "sess_t", WorkspaceID: "ws", UserEventID: "u", UserMessage: "x",
	})
	// Two non-terminal past events (production hardcodes 'client_event').
	payloadA, _ := json.Marshal(map[string]string{"text": "alpha"})
	payloadB, _ := json.Marshal(map[string]string{"text": "beta"})
	if _, err := store.InsertEventsWithTurn(context.Background(), "sess_t", 0, "trn_t", []EventInput{
		{EventID: "evt_a", Payload: payloadA},
		{EventID: "evt_b", Payload: payloadB},
	}); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	// Mark turn done BEFORE the request so the post-loop GetTurn sees terminal.
	_ = store.MarkTurnDone(context.Background(), "trn_t")

	srv := &Server{
		store: store, sse: sse,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r := chi.NewRouter()
	r.Get("/api/turns/{tid}/events", srv.handleTurnEvents)

	req := httptest.NewRequest("GET", "/api/turns/trn_t/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { r.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("handler did not return promptly for terminal turn with past events")
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"event_id":"evt_a"`) {
		t.Fatalf("expected evt_a in stream, got %s", out)
	}
	if !strings.Contains(out, `"event_id":"evt_b"`) {
		t.Fatalf("expected evt_b in stream, got %s", out)
	}
	if !strings.Contains(out, `"event_type":"done"`) {
		t.Fatalf("expected done sentinel, got %s", out)
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
