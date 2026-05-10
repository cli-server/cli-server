package codexappgateway

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
)

// makeFakeCodex compiles a small Go program that mimics the bits of
// `codex app-server` we depend on: print "ws://127.0.0.1:PORT" on
// stdout, then serve /readyz on that port.
func makeFakeCodex(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	const program = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
)

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	addr := l.Addr().(*net.TCPAddr)
	fmt.Printf("ws://%s\n", addr.String())
	http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	_ = http.Serve(l, nil)
}
`
	if err := os.WriteFile(src, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-codex")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("build fake codex: %v\n%s", err, out)
	}
	return bin
}

// inMemStore implements codexhome.ObjectStore in-memory for tests.
type inMemStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (f *inMemStore) Put(_ context.Context, k string, d []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[k] = append([]byte(nil), d...)
	return nil
}
func (f *inMemStore) Get(_ context.Context, k string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.m[k]
	if !ok {
		return nil, codexhome.ErrObjectNotFound
	}
	return append([]byte(nil), d...), nil
}
func (f *inMemStore) Delete(_ context.Context, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, k)
	return nil
}

func makeFakeStore(_ *testing.T) codexhome.ObjectStore {
	return &inMemStore{m: map[string][]byte{}}
}
