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
			Payload:   json.RawMessage(`{"text":"hi"}`),
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		})
		_ = store.MarkTurnDone(context.Background(), t.ID)
		sse.Publish(t.SessionID, &StreamClientEvent{
			EventID: "evt_term", EventType: "turn_done", TurnID: t.ID,
			Payload:   json.RawMessage(`{}`),
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
