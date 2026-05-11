package supervisor

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
)

// fakeStore implements codexhome.ObjectStore in-memory.
type fakeStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string][]byte{}} }

func (f *fakeStore) Put(_ context.Context, k string, d []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[k] = append([]byte(nil), d...)
	return nil
}
func (f *fakeStore) Get(_ context.Context, k string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.m[k]
	if !ok {
		return nil, codexhome.ErrObjectNotFound
	}
	return append([]byte(nil), d...), nil
}
func (f *fakeStore) Delete(_ context.Context, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, k)
	return nil
}

func defaultConfigInput() codexhome.ConfigInput {
	return codexhome.ConfigInput{
		ModelProvider:  "p",
		Model:          "m",
		ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
	}
}

func TestSupervisor_EnsureSubprocess_SpawnsOnce(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{
		CodexBin: bin,
		HomeMgr:  mgr,
		Store:    store,
	})
	defer sup.ShutdownAll(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	build := func() (codexhome.ConfigInput, error) { return defaultConfigInput(), nil }
	key := Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}
	h1, err := sup.EnsureSubprocess(ctx, key, build)
	if err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	h2, err := sup.EnsureSubprocess(ctx, key, build)
	if err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if h1.WSURL != h2.WSURL {
		t.Errorf("two ensures returned different handles: %s vs %s", h1.WSURL, h2.WSURL)
	}
}

func TestSupervisor_Shutdown_UploadsToS3(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	build := func() (codexhome.ConfigInput, error) { return defaultConfigInput(), nil }
	key := Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}
	h, err := sup.EnsureSubprocess(ctx, key, build)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.CodexHome+"/sessions", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.CodexHome+"/sessions/x.jsonl", []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sup.Shutdown(ctx, key); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	wantKey := "codex-app-gateway/ws_a/thr_1.tar.gz"
	if _, ok := store.m[wantKey]; !ok {
		t.Fatalf("no S3 object at %s; have: %v", wantKey, keysOf(store.m))
	}
}

func TestSupervisor_EnsureSubprocess_RestoresFromS3(t *testing.T) {
	bin := buildFakeCodex(t)
	store := newFakeStore()

	{
		mgr := codexhome.NewManager(t.TempDir())
		sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		build := func() (codexhome.ConfigInput, error) { return defaultConfigInput(), nil }
		key := Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}
		h, err := sup.EnsureSubprocess(ctx, key, build)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(h.CodexHome+"/marker.txt", []byte("from-pass-1"), 0o600)
		_ = sup.Shutdown(ctx, key)
	}

	sup2 := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: codexhome.NewManager(t.TempDir()), Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	build := func() (codexhome.ConfigInput, error) { return defaultConfigInput(), nil }
	h, err := sup2.EnsureSubprocess(ctx, Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}, build)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	defer sup2.ShutdownAll(context.Background())
	got, err := os.ReadFile(h.CodexHome + "/marker.txt")
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if string(got) != "from-pass-1" {
		t.Errorf("marker = %q", got)
	}
}

func TestSupervisor_Ensure_BuildError_PropagatesAndDoesNotSpawn(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wantErr := errors.New("nope")
	_, err := sup.EnsureSubprocess(ctx, Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}, func() (codexhome.ConfigInput, error) {
		return codexhome.ConfigInput{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("want wantErr, got %v", err)
	}
}

func TestSupervisor_EnsureSubprocess_RespawnsAfterCrash(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	defer sup.ShutdownAll(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	build := func() (codexhome.ConfigInput, error) { return defaultConfigInput(), nil }
	key := Key{WorkspaceID: "ws_a", ThreadID: "thr_crash"}

	h1, err := sup.EnsureSubprocess(ctx, key, build)
	if err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	// Simulate crash: SIGKILL the fake-codex process.
	if err := h1.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// Wait for the done channel to close (the wait goroutine in spawn.go
	// observes the exit and closes done).
	<-h1.Done()
	if h1.IsAlive() {
		t.Fatal("IsAlive returned true after kill+Done")
	}

	h2, err := sup.EnsureSubprocess(ctx, key, build)
	if err != nil {
		t.Fatalf("ensure 2 (after crash): %v", err)
	}
	if h1.WSURL == h2.WSURL {
		t.Errorf("expected fresh subprocess, got same URL %s", h1.WSURL)
	}
	if !h2.IsAlive() {
		t.Error("respawned handle should be alive")
	}
}

func keysOf(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
