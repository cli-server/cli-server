package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOperationsInsertAndList(t *testing.T) {
	d := newTestDB(t)
	t.Cleanup(func() { d.Exec(`DELETE FROM operations WHERE workspace_id = $1`, "ws-1-"+t.Name()) })

	ws := "ws-1-" + t.Name()
	op := Operation{
		ID:            uuid.NewString(),
		WorkspaceID:   ws,
		UserID:        opPtr("u-1"),
		Source:        "sdk",
		ThreadID:      opPtr("th-1"),
		RequestID:     opPtr("rpc-1"),
		EnvID:         "alpha",
		Tool:          "shell",
		Arguments:     json.RawMessage(`{"command":"ls"}`),
		IsError:       false,
		ResultSummary: opPtr("ok"),
		StartedAt:     time.Now().UTC().Truncate(time.Microsecond),
		CompletedAt:   time.Now().UTC().Truncate(time.Microsecond),
		DurationMs:    7,
	}
	if err := d.InsertOperation(op); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := d.ListOperations(OperationFilter{WorkspaceID: ws, Limit: 10})
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
	d := newTestDB(t)
	ws := "ws-1-" + t.Name()
	t.Cleanup(func() { d.Exec(`DELETE FROM operations WHERE workspace_id = $1`, ws) })

	must := func(env string) {
		err := d.InsertOperation(Operation{
			ID: uuid.NewString(), WorkspaceID: ws, Source: "sdk",
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

	rows, err := d.ListOperations(OperationFilter{WorkspaceID: ws, EnvID: "alpha", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("alpha rows = %d, want 2", len(rows))
	}
}

func TestOperationsPruneOlderThan(t *testing.T) {
	d := newTestDB(t)
	ws := "ws-prune-" + t.Name()
	t.Cleanup(func() { d.Exec(`DELETE FROM operations WHERE workspace_id = $1`, ws) })

	oldT := time.Now().UTC().Add(-100 * 24 * time.Hour)
	newT := time.Now().UTC()
	for _, st := range []time.Time{oldT, newT} {
		err := d.InsertOperation(Operation{
			ID: uuid.NewString(), WorkspaceID: ws, Source: "sdk",
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
	if n < 1 {
		t.Fatalf("pruned = %d, want >= 1", n)
	}
}

func opPtr[T any](v T) *T { return &v }
