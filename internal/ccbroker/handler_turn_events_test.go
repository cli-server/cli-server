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
