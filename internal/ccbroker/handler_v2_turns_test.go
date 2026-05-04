package ccbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleProcessTurnV2_Returns202WithTurnID(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	registry := newWorkerRegistry(workerDeps{
		store: store, sse: sse,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	registry.executeOverride = func(context.Context, *AgentTurn) {} // no-op
	defer registry.Shutdown(context.Background())

	srv := &Server{
		store: store, sse: sse,
		activeTurns:    newActiveTurnRegistry(),
		compactQueue:   newCompactQueue(),
		workerRegistry: registry,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	body := bytes.NewBufferString(`{"session_id":"sess_v","workspace_id":"ws","user_message":"hi"}`)
	req := httptest.NewRequest("POST", "/api/v2/turns", body)
	rec := httptest.NewRecorder()
	srv.handleProcessTurnV2(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		TurnID    string `json:"turn_id"`
		EventsURL string `json:"events_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TurnID == "" {
		t.Fatalf("turn_id missing")
	}
	if resp.EventsURL != "/api/turns/"+resp.TurnID+"/events" {
		t.Fatalf("events_url=%q", resp.EventsURL)
	}
	turns, _ := store.ListSessionTurns(context.Background(), "sess_v", 10)
	if len(turns) != 1 || turns[0].ID != resp.TurnID {
		t.Fatalf("turn not enqueued under returned id: %+v", turns)
	}
}

func TestHandleProcessTurnV2_DepthLimit(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	registry := newWorkerRegistry(workerDeps{
		store: store, sse: sse,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	registry.executeOverride = func(context.Context, *AgentTurn) {}
	defer registry.Shutdown(context.Background())

	srv := &Server{
		store: store, sse: sse,
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		workerRegistry: registry,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	store.sessions["sess_full"] = &Session{ID: "sess_full", WorkspaceID: "ws"}
	for i := 0; i < maxPendingPerSession; i++ {
		_ = store.EnqueueTurn(context.Background(), AgentTurn{
			ID: "preexist_" + string(rune('a'+i)), SessionID: "sess_full", WorkspaceID: "ws",
			UserEventID: "u", UserMessage: "x",
		})
	}

	body := bytes.NewBufferString(`{"session_id":"sess_full","workspace_id":"ws","user_message":"overflow"}`)
	req := httptest.NewRequest("POST", "/api/v2/turns", body)
	rec := httptest.NewRecorder()
	srv.handleProcessTurnV2(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
}

func TestHandleProcessTurnV2_RespectsCallerTurnID(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	registry := newWorkerRegistry(workerDeps{
		store: store, sse: sse,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	registry.executeOverride = func(context.Context, *AgentTurn) {}
	defer registry.Shutdown(context.Background())
	srv := &Server{store: store, sse: sse,
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		workerRegistry: registry,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil))}
	body := bytes.NewBufferString(`{"session_id":"sess_x","workspace_id":"ws","user_message":"hi","turn_id":"trn_supplied"}`)
	req := httptest.NewRequest("POST", "/api/v2/turns", body)
	rec := httptest.NewRecorder()
	srv.handleProcessTurnV2(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		TurnID string `json:"turn_id"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.TurnID != "trn_supplied" {
		t.Fatalf("turn_id=%q", resp.TurnID)
	}
}

func TestHandleProcessTurnV2_ValidationFailure(t *testing.T) {
	srv := &Server{
		store: newFakeStore(), sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(), compactQueue: newCompactQueue(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body := bytes.NewBufferString(`{"workspace_id":"ws"}`) // missing session_id + user_message
	req := httptest.NewRequest("POST", "/api/v2/turns", body)
	rec := httptest.NewRecorder()
	srv.handleProcessTurnV2(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
