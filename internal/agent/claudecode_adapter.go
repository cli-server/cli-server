package agent

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// ClaudeCodePTY manages a Claude Code process running inside a pseudo-terminal.
type ClaudeCodePTY struct {
	ptmx *os.File
	cmd  *exec.Cmd
	mu   sync.Mutex
}

// NewClaudeCodePTY starts a Claude Code process in a PTY.
func NewClaudeCodePTY(claudeBin, workDir string, cols, rows uint16) (*ClaudeCodePTY, error) {
	cmd := exec.Command(claudeBin)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if workDir != "" {
		cmd.Dir = workDir
	}

	winSize := &pty.Winsize{Cols: cols, Rows: rows}
	if cols == 0 || rows == 0 {
		winSize = &pty.Winsize{Cols: 120, Rows: 40}
	}

	ptmx, err := pty.StartWithSize(cmd, winSize)
	if err != nil {
		return nil, err
	}

	return &ClaudeCodePTY{ptmx: ptmx, cmd: cmd}, nil
}

func (p *ClaudeCodePTY) Read(b []byte) (int, error)  { return p.ptmx.Read(b) }
func (p *ClaudeCodePTY) Write(b []byte) (int, error) { return p.ptmx.Write(b) }

// Resize changes the PTY window size.
func (p *ClaudeCodePTY) Resize(cols, rows uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return pty.Setsize(p.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// Close terminates the process and closes the PTY.
func (p *ClaudeCodePTY) Close() error {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	return p.ptmx.Close()
}

// BridgeTerminalStream bridges a yamux terminal stream to a PTY.
//
// Protocol on the stream:
//   - Prefix 0x00 + data: terminal I/O bytes
//   - Prefix 0x01 + JSON: control commands (resize, etc.)
func BridgeTerminalStream(stream net.Conn, p *ClaudeCodePTY) {
	done := make(chan struct{})

	// PTY → stream
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			if err != nil {
				return
			}
			// Prefix 0x00 = data frame
			frame := make([]byte, 1+n)
			frame[0] = 0x00
			copy(frame[1:], buf[:n])
			if _, err := stream.Write(frame); err != nil {
				return
			}
		}
	}()

	// stream → PTY
	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		if err != nil {
			break
		}
		if n == 0 {
			continue
		}
		switch buf[0] {
		case 0x01:
			// Control command
			var cmd struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(buf[1:n], &cmd) == nil && cmd.Type == "resize" {
				if err := p.Resize(cmd.Cols, cmd.Rows); err != nil {
					log.Printf("pty resize error: %v", err)
				}
			}
		default:
			// Data frame (0x00 prefix or any other)
			data := buf[1:n]
			if len(data) > 0 {
				p.Write(data)
			}
		}
	}

	<-done
}
