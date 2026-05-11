package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildFakeCodex compiles a small Go program that mimics the bits of
// `codex app-server` we depend on: print "ws://127.0.0.1:PORT" on
// stdout, then serve /readyz on that port.
func buildFakeCodex(t *testing.T) string {
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

func TestSpawnCodexAppServer_HappyPath(t *testing.T) {
	bin := buildFakeCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := spawnCodexAppServer(ctx, bin, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer h.Stop(context.Background())
	if !strings.HasPrefix(h.WSURL, "ws://127.0.0.1:") {
		t.Errorf("WSURL = %s", h.WSURL)
	}
	if !strings.HasPrefix(h.HTTPURL, "http://127.0.0.1:") {
		t.Errorf("HTTPURL = %s", h.HTTPURL)
	}
}

func TestSpawnCodexAppServer_BadBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := spawnCodexAppServer(ctx, "/no/such/binary", t.TempDir(), nil)
	if err == nil {
		t.Fatal("want spawn error")
	}
}
