package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupAndTeardown_RoundTrip(t *testing.T) {
	old := TempDirBase
	TempDirBase = t.TempDir()
	defer func() { TempDirBase = old }()

	fake := newFakeS3("ccbroker")
	// Pre-load one file so the first Setup has something to download.
	fake.objects[claudeHomeKey("ws1")] = makeTarGz(t, map[string]string{
		"CLAUDE.md": "global-claude",
	})

	store, srv := newTestStore(t, fake)
	defer srv.Close()

	ctx := context.Background()
	ws, err := Setup(ctx, "ws1", "cse_abc", store)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Downloaded file present in ClaudeDir.
	got, err := os.ReadFile(filepath.Join(ws.ClaudeDir, "CLAUDE.md"))
	if err != nil || string(got) != "global-claude" {
		t.Fatalf("CLAUDE.md mismatch: got=%q err=%v", got, err)
	}
	// Memory dir created at the deterministic path.
	wantMem := filepath.Join(ws.ClaudeDir, "projects", "ws_ws1", "memory")
	if _, err := os.Stat(wantMem); err != nil {
		t.Fatalf("memory dir missing: %v", err)
	}

	// Mutate one file + add a new one. Teardown must upload everything.
	if err := os.WriteFile(filepath.Join(ws.ClaudeDir, "CLAUDE.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.MemoryDir, "MEMORY.md"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Teardown(ctx, ws, store); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	// TempDir gone.
	if _, err := os.Stat(ws.TempDir); !os.IsNotExist(err) {
		t.Fatalf("TempDir should be removed; err=%v", err)
	}

	// One object uploaded under this workspace's key.
	if _, ok := fake.uploads[claudeHomeKey("ws1")]; !ok {
		t.Fatalf("expected upload at %s, uploads=%v", claudeHomeKey("ws1"), keysOf(fake.uploads))
	}

	// Round-trip: stage the upload as the new object, fresh Setup gets the
	// mutated content back.
	fake.objects[claudeHomeKey("ws1")] = fake.uploads[claudeHomeKey("ws1")]
	ws2, err := Setup(ctx, "ws1", "cse_abc", store)
	if err != nil {
		t.Fatalf("Setup #2: %v", err)
	}
	defer Teardown(ctx, ws2, store)
	got2, _ := os.ReadFile(filepath.Join(ws2.ClaudeDir, "CLAUDE.md"))
	if string(got2) != "changed" {
		t.Fatalf("post-roundtrip CLAUDE.md: got %q want %q", got2, "changed")
	}
	got2, _ = os.ReadFile(filepath.Join(ws2.MemoryDir, "MEMORY.md"))
	if string(got2) != "note" {
		t.Fatalf("post-roundtrip MEMORY.md: got %q want %q", got2, "note")
	}
}

func TestSetup_EmptyWorkspaceWhenObjectMissing(t *testing.T) {
	old := TempDirBase
	TempDirBase = t.TempDir()
	defer func() { TempDirBase = old }()

	fake := newFakeS3("ccbroker")
	store, srv := newTestStore(t, fake)
	defer srv.Close()

	ws, err := Setup(context.Background(), "ws_new", "cse_x", store)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer Teardown(context.Background(), ws, store)

	// ClaudeDir exists but has no files (only the memory subtree we mkdir).
	entries, err := os.ReadDir(ws.ClaudeDir)
	if err != nil {
		t.Fatal(err)
	}
	// Only the "projects" directory we created for MemoryDir.
	if len(entries) != 1 || entries[0].Name() != "projects" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("ClaudeDir should only contain the projects/ scaffold; got %v", names)
	}
}

func TestTeardown_UploadFailureStillCleansTempDir(t *testing.T) {
	old := TempDirBase
	TempDirBase = t.TempDir()
	defer func() { TempDirBase = old }()

	fake := newFakeS3("ccbroker")
	store, srv := newTestStore(t, fake)
	defer srv.Close()

	ws, err := Setup(context.Background(), "ws_fail", "cse_y", store)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Now make the upstream PUT fail. Teardown must log + continue, returning
	// nil and removing TempDir even though the upload failed.
	fake.failPUT = true

	if err := Teardown(context.Background(), ws, store); err != nil {
		t.Fatalf("Teardown: want nil even when upload fails, got %v", err)
	}
	if _, err := os.Stat(ws.TempDir); !os.IsNotExist(err) {
		t.Fatalf("TempDir should be removed even after upload failure; err=%v", err)
	}
	if len(fake.uploads) != 0 {
		t.Fatalf("no upload should have been recorded; got %d", len(fake.uploads))
	}
}
