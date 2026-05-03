package ccbroker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/ccbroker/tools"
	"github.com/google/uuid"
)

// newFakeServerWithRealGate builds a Server with the same SSE-wired gate
// notifier that NewServer uses in production. This allows tests to verify that
// permission events actually reach SSE subscribers.
func newFakeServerWithRealGate(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		config:       Config{},
		store:        newFakeStore(),
		sse:          NewSSEBroker(),
		turnLock:     NewTurnLock(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		activeTurns:  newActiveTurnRegistry(),
		compactQueue: newCompactQueue(),
	}
	// Wire the gate with the real SSE publish path (same as NewServer).
	s.gate = tools.NewGate(func(sid string, e tools.Event) {
		payload, err := json.Marshal(e)
		if err != nil {
			s.logger.Warn("permission event marshal failed",
				"session_id", sid, "type", e.Type, "err", err)
			return
		}
		s.sse.Publish(sid, &StreamClientEvent{
			EventID:   "evt_" + uuid.NewString(),
			EventType: e.Type,
			Source:    "gate",
			Payload:   payload,
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		})
	})
	return s
}

func TestPermissionEventReachesSSE(t *testing.T) {
	s := newFakeServerWithRealGate(t)

	sessionID := "cse_perm_sse_test"

	// Subscribe to SSE events for the session before triggering Check.
	sub := s.sse.Subscribe(sessionID)
	defer s.sse.Unsubscribe(sessionID, sub)

	// Trigger Gate.Check in ask mode in a goroutine — it will emit
	// permission_request and block until resolved or timeout.
	checkDone := make(chan struct{})
	go func() {
		defer close(checkDone)
		_ = s.gate.Check(context.Background(), tools.CheckRequest{
			SessionID:            sessionID,
			TurnID:               "trn_test",
			Tool:                 "remote_bash",
			ExecutorID:           "exe_a",
			Args:                 json.RawMessage(`{"command":"ls"}`),
			PermissionMode:       "ask",
			SessionCreatorUserID: "u",
			ExecutorOwnerUserID:  "u",
			Timeout:              200 * time.Millisecond,
		})
	}()

	// Verify a permission_request StreamClientEvent arrives on the SSE channel.
	var firstEv *StreamClientEvent
	select {
	case ev := <-sub.Ch:
		firstEv = ev
	case <-time.After(time.Second):
		t.Fatal("no permission_request event reached SSE within 1s")
	}
	if firstEv.EventType != "permission_request" {
		t.Errorf("first event type=%q want permission_request", firstEv.EventType)
	}
	if firstEv.Source != "gate" {
		t.Errorf("source=%q want gate", firstEv.Source)
	}
	var payload map[string]any
	if err := json.Unmarshal(firstEv.Payload, &payload); err != nil {
		t.Errorf("payload not valid JSON: %v", err)
	}
	// Event struct fields now marshal with snake_case json tags.
	if payload["permission_id"] == "" || payload["permission_id"] == nil {
		t.Errorf("permission_id missing from payload: %v", payload)
	}

	// After timeout (200ms), a permission_resolved event should arrive.
	select {
	case ev := <-sub.Ch:
		if ev.EventType != "permission_resolved" {
			t.Errorf("second event type=%q want permission_resolved", ev.EventType)
		}
		if ev.Source != "gate" {
			t.Errorf("source=%q want gate", ev.Source)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no permission_resolved event reached SSE within 2s after timeout")
	}

	<-checkDone
}
