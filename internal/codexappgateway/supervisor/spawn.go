// Package supervisor spawns and tracks per-thread `codex app-server`
// subprocesses inside the codex-app-gateway pod.
package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ChildHandle is what spawnCodexAppServer returns.
type ChildHandle struct {
	Cmd       *exec.Cmd
	WSURL     string // ws://127.0.0.1:PORT
	HTTPURL   string // http://127.0.0.1:PORT  (for /readyz, /healthz)
	CodexHome string
}

// Stop sends SIGTERM, waits up to 10s, then SIGKILLs.
func (h *ChildHandle) Stop(ctx context.Context) error {
	if h.Cmd == nil || h.Cmd.Process == nil {
		return nil
	}
	if err := h.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.Cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		_ = h.Cmd.Process.Signal(syscall.SIGKILL)
		<-done
		return nil
	case <-ctx.Done():
		_ = h.Cmd.Process.Signal(syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

// spawnCodexAppServer launches `codexBin app-server --listen ws://127.0.0.1:0`,
// reads the listen URL from its output, polls /readyz, and returns a handle.
//
// The real `codex` binary writes a multi-line startup banner to stderr:
//
//	codex app-server (WebSockets)
//	  listening on: ws://127.0.0.1:PORT
//	  readyz: http://127.0.0.1:PORT/readyz
//	  ...
//
// Test fakes write a bare "ws://127.0.0.1:PORT\n" to stdout. We scan both
// streams concurrently and extract the first line containing "ws://".
func spawnCodexAppServer(ctx context.Context, codexBin, codexHome string, extraEnv []string) (*ChildHandle, error) {
	cmd := exec.Command(codexBin, "app-server", "--listen", "ws://127.0.0.1:0")
	cmd.Env = append(append([]string{}, extraEnv...), "CODEX_HOME="+codexHome)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	// Scan both stdout and stderr concurrently for a line containing "ws://".
	// Real codex writes the URL to stderr; test fakes write it to stdout.
	type result struct {
		url string
		err error
	}
	urlCh := make(chan result, 1)
	scanStream := func(r io.Reader) {
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			trimmed := strings.TrimSpace(line)
			if idx := strings.Index(trimmed, "ws://"); idx >= 0 {
				select {
				case urlCh <- result{url: trimmed[idx:]}:
				default:
				}
				// Drain remainder in background.
				go func() { _, _ = io.Copy(io.Discard, br) }()
				return
			}
			if err != nil {
				select {
				case urlCh <- result{err: err}:
				default:
				}
				return
			}
		}
	}
	go scanStream(stdout)
	go scanStream(stderr)

	var wsURL string
	select {
	case r := <-urlCh:
		if r.err != nil {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("read listen line: %w", r.err)
		}
		wsURL = r.url
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, ctx.Err()
	}

	if !strings.HasPrefix(wsURL, "ws://") {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("unexpected listen line %q", wsURL)
	}
	httpURL := "http://" + strings.TrimPrefix(wsURL, "ws://")
	// Both pipes are drained in background goroutines already.

	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", httpURL+"/readyz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("readyz never returned 200: last err=%v", err)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return &ChildHandle{Cmd: cmd, WSURL: wsURL, HTTPURL: httpURL, CodexHome: codexHome}, nil
}
