package codexhome

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeS3 is an in-memory ObjectStore used by the test suite.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) Put(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
	return nil
}

func (f *fakeS3) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeS3) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func TestS3RoundTrip_TarUntarPreservesContents(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	src, err := m.NewTmpDir("ws_a", "thr_1")
	if err != nil {
		t.Fatalf("NewTmpDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "config.toml"), []byte("model = \"m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sessions", "x.jsonl"), []byte(`{"a":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := newFakeS3()
	backend := NewS3Backend(store, "ws_a", "thr_1")
	if err := backend.Upload(context.Background(), src); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, ok := store.objects[backend.Key()]; !ok {
		t.Fatalf("object missing: %v", store.objects)
	}

	// Recreate empty dir; download should re-populate.
	dst := filepath.Join(t.TempDir(), "ws_a", "thr_1")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := backend.Download(context.Background(), dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "config.toml")); string(got) != "model = \"m\"\n" {
		t.Errorf("config.toml = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "sessions", "x.jsonl")); string(got) != `{"a":1}`+"\n" {
		t.Errorf("sessions/x.jsonl = %q", got)
	}
}

func TestS3Backend_Download_NotFound_IsRecognizable(t *testing.T) {
	store := newFakeS3()
	backend := NewS3Backend(store, "ws_a", "thr_missing")
	err := backend.Download(context.Background(), t.TempDir())
	if !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}
