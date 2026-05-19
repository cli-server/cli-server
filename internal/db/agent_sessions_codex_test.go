package db

import (
	"context"
	"testing"
)

func TestCodexThreadIDRoundTrip(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	sid := "cse_codex_" + t.Name()
	const wsID = "ws_codex_test"
	const extID = "chat-codex-1@im.wechat"

	if err := d.CreateAgentSessionTUI(ctx, CreateTUISessionParams{
		ID:            sid,
		WorkspaceID:   wsID,
		ExternalID:    extID,
		Title:         "codex thread id test",
		CreatorUserID: "u_codex",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM agent_sessions WHERE id = $1`, sid) })

	// Initially codex_thread_id should be nil.
	got, err := d.GetSessionByExternalID(ctx, wsID, extID)
	if err != nil {
		t.Fatalf("get (initial): %v", err)
	}
	if got == nil {
		t.Fatal("session not found after create")
	}
	if got.CodexThreadID != nil {
		t.Errorf("initial CodexThreadID = %v, want nil", got.CodexThreadID)
	}

	// Set a thread ID.
	tid := "thr-abc123"
	if err := d.SetSessionCodexThreadID(ctx, sid, &tid); err != nil {
		t.Fatalf("SetSessionCodexThreadID (set): %v", err)
	}
	got, err = d.GetSessionByExternalID(ctx, wsID, extID)
	if err != nil {
		t.Fatalf("get (after set): %v", err)
	}
	if got.CodexThreadID == nil || *got.CodexThreadID != "thr-abc123" {
		t.Errorf("after set: CodexThreadID = %v, want thr-abc123", got.CodexThreadID)
	}

	// Clear the thread ID (nil = clear).
	if err := d.SetSessionCodexThreadID(ctx, sid, nil); err != nil {
		t.Fatalf("SetSessionCodexThreadID (clear): %v", err)
	}
	got, err = d.GetSessionByExternalID(ctx, wsID, extID)
	if err != nil {
		t.Fatalf("get (after clear): %v", err)
	}
	if got.CodexThreadID != nil {
		t.Errorf("after clear: CodexThreadID = %v, want nil", got.CodexThreadID)
	}
}
