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
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	// Seed: 1 old + 1 new
	ws := "ws-retention-" + t.Name()
	old := time.Now().UTC().Add(-100 * 24 * time.Hour)
	now := time.Now().UTC()
	for _, ts := range []time.Time{old, now} {
		err := s.DB.InsertOperation(db.Operation{
			ID: uuid.NewString(), WorkspaceID: ws, Source: "sdk",
			EnvID: "a", Tool: "shell", IsError: false,
			Arguments: json.RawMessage(`{}`),
			StartedAt: ts, CompletedAt: ts, DurationMs: 1,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.DB.Exec("DELETE FROM operations WHERE workspace_id = $1", ws)
	})

	n, err := s.runRetentionOnce(90 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if n < 1 {
		t.Fatalf("deleted = %d, want >= 1", n)
	}

	// Newer row survives
	rows, err := s.DB.ListOperations(db.OperationFilter{WorkspaceID: ws, Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("remaining = %d, want 1", len(rows))
	}
}

func TestStartRetention_TickerStops(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

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

func TestStartRetention_ZeroTTLDisables(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// With ttl=0 the loop returns immediately
	done := make(chan struct{})
	go func() {
		s.startRetentionLoop(ctx, 0, 50*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("zero TTL should disable the loop and return immediately")
	}
}
