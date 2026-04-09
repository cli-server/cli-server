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
		p.cmd.Wait() // reap the zombie process
	}
	return p.ptmx.Close()
}

// IsAlive reports whether the underlying process is still running.
func (p *ClaudeCodePTY) IsAlive() bool {
	return p.cmd.ProcessState == nil // nil means Wait() hasn't returned yet
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

const replayBufferSize = 256 * 1024 // 256KB replay buffer

// TerminalMux multiplexes a single PTY to one active stream at a time,
// maintaining a replay buffer so reconnecting clients see previous output.
type TerminalMux struct {
	pty    *ClaudeCodePTY
	mu     sync.Mutex
	active net.Conn   // currently active stream (nil if none)
	buf    []byte     // circular replay buffer contents
	done   chan struct{}
}

// NewTerminalMux creates a multiplexer for the given PTY and starts
// reading PTY output in the background.
func NewTerminalMux(p *ClaudeCodePTY) *TerminalMux {
	m := &TerminalMux{
		pty:  p,
		done: make(chan struct{}),
	}
	go m.readLoop()
	return m
}

// readLoop continuously reads PTY output, appends to the replay buffer,
// and forwards to the active stream.
func (m *TerminalMux) readLoop() {
	defer close(m.done)
	buf := make([]byte, 4096)
	for {
		n, err := m.pty.Read(buf)
		if err != nil {
			return
		}
		data := buf[:n]

		// Buffer and snapshot the active stream under the lock,
		// but do the actual Write outside to avoid blocking Attach().
		m.mu.Lock()
		m.buf = append(m.buf, data...)
		if len(m.buf) > replayBufferSize {
			m.buf = m.buf[len(m.buf)-replayBufferSize:]
		}
		s := m.active
		m.mu.Unlock()

		if s != nil {
			frame := make([]byte, 1+n)
			frame[0] = 0x00
			copy(frame[1:], data)
			if _, err := s.Write(frame); err != nil {
				m.mu.Lock()
				if m.active == s {
					m.active = nil
				}
				m.mu.Unlock()
			}
		}
	}
}

// Attach connects a new terminal stream. The previous stream (if any)
// is closed, the replay buffer is sent to the new stream, and input
// from the new stream is forwarded to the PTY.
func (m *TerminalMux) Attach(stream net.Conn) {
	m.mu.Lock()
	if m.active != nil {
		m.active.Close()
	}
	m.active = stream

	// Snapshot buffer for replay outside the lock.
	var replay []byte
	if len(m.buf) > 0 {
		replay = make([]byte, len(m.buf))
		copy(replay, m.buf)
	}
	m.mu.Unlock()

	// Replay buffered output (outside lock to avoid blocking readLoop).
	if len(replay) > 0 {
		frame := make([]byte, 1+len(replay))
		frame[0] = 0x00
		copy(frame[1:], replay)
		stream.Write(frame)
	}

	// Read input from stream → PTY (blocks until stream closes).
	inputBuf := make([]byte, 4096)
	for {
		n, err := stream.Read(inputBuf)
		if err != nil {
			break
		}
		if n == 0 {
			continue
		}
		switch inputBuf[0] {
		case 0x01:
			var cmd struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(inputBuf[1:n], &cmd) == nil && cmd.Type == "resize" {
				if err := m.pty.Resize(cmd.Cols, cmd.Rows); err != nil {
					log.Printf("pty resize error: %v", err)
				}
			}
		default:
			data := inputBuf[1:n]
			if len(data) > 0 {
				m.pty.Write(data)
			}
		}
	}

	// Detach this stream if it's still the active one.
	m.mu.Lock()
	if m.active == stream {
		m.active = nil
	}
	m.mu.Unlock()
}

// IsAlive reports whether the underlying PTY is still running.
func (m *TerminalMux) IsAlive() bool {
	return m.pty.IsAlive()
}

// Close shuts down the PTY and waits for the read loop to finish.
func (m *TerminalMux) Close() {
	m.mu.Lock()
	if m.active != nil {
		m.active.Close()
		m.active = nil
	}
	m.mu.Unlock()
	m.pty.Close()
	<-m.done
}
