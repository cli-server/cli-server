package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// OpencodeProcess manages a local opencode serve subprocess.
type OpencodeProcess struct {
	cmd  *exec.Cmd
	Port int
}

// StartOpencode starts "opencode serve --hostname 127.0.0.1 --port {port}" as a child process.
// It returns immediately after starting the process.
func StartOpencode(bin string, port int) (*OpencodeProcess, error) {
	cmd := exec.Command(bin, "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start opencode: %w", err)
	}

	log.Printf("Started opencode (pid %d) on port %d", cmd.Process.Pid, port)
	return &OpencodeProcess{cmd: cmd, Port: port}, nil
}

// WaitReady polls http://localhost:{port}/ every 500ms until a response is received or the timeout expires.
func (p *OpencodeProcess) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://localhost:%d/", p.Port)
	client := &http.Client{Timeout: 2 * time.Second}

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			log.Printf("opencode is ready on port %d", p.Port)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	return fmt.Errorf("opencode did not become ready within %s", timeout)
}

// Stop sends SIGTERM to the child process, waits briefly, then sends SIGKILL if needed.
func (p *OpencodeProcess) Stop() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	log.Printf("Stopping opencode (pid %d)...", p.cmd.Process.Pid)

	_ = p.cmd.Process.Signal(os.Interrupt)

	done := make(chan struct{})
	go func() {
		p.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("opencode stopped gracefully")
	case <-time.After(5 * time.Second):
		log.Println("opencode did not stop in time, sending SIGKILL")
		_ = p.cmd.Process.Kill()
		<-done
	}
}
