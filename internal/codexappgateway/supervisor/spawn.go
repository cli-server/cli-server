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
// reads the listen URL from stdout, polls /readyz, and returns a handle.
func spawnCodexAppServer(ctx context.Context, codexBin, codexHome string, extraEnv []string) (*ChildHandle, error) {
	cmd := exec.Command(codexBin, "app-server", "--listen", "ws://127.0.0.1:0")
	cmd.Env = append(append([]string{}, extraEnv...), "CODEX_HOME="+codexHome)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	br := bufio.NewReader(stdout)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("read listen line: %w", err)
	}
	wsURL := strings.TrimSpace(line)
	if !strings.HasPrefix(wsURL, "ws://") {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("unexpected first stdout line %q", wsURL)
	}
	httpURL := "http://" + strings.TrimPrefix(wsURL, "ws://")
	// Drain remaining stdout in the background so the pipe doesn't fill
	// (real codex keeps logging readyz/healthz/notes after the URL line).
	go func() { _, _ = io.Copy(io.Discard, br) }()

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
