package ws

import (
	"encoding/binary"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/imryao/cli-server/internal/process"
	"github.com/imryao/cli-server/internal/session"
)

const (
	// Client → Server message types
	MsgInput  byte = 0
	MsgResize byte = 1
	MsgPing   byte = 2

	// Server → Client message types
	MsgOutput byte = 0
	MsgPong   byte = 1
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Handler struct {
	Sessions       *session.Store
	ProcessManager process.Manager
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, sessionID string) {
	sess, ok := h.Sessions.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	p, ok := h.ProcessManager.Get(sessionID)
	if !ok {
		http.Error(w, "process not found", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Send buffered output for reconnection
	buffered := sess.Output.Bytes()
	if len(buffered) > 0 {
		msg := append([]byte{MsgOutput}, buffered...)
		conn.WriteMessage(websocket.BinaryMessage, msg)
	}

	// PTY → WebSocket
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				data := buf[:n]
				sess.Output.Write(data)
				msg := append([]byte{MsgOutput}, data...)
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, msg); writeErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("pty read error: %v", err)
				}
				return
			}
		}
	}()

	// WebSocket → PTY
	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if len(message) == 0 {
				continue
			}
			msgType := message[0]
			payload := message[1:]

			switch msgType {
			case MsgInput:
				p.Write(payload)
			case MsgResize:
				if len(payload) >= 4 {
					cols := binary.BigEndian.Uint16(payload[0:2])
					rows := binary.BigEndian.Uint16(payload[2:4])
					p.Resize(rows, cols)
				}
			case MsgPing:
				conn.WriteMessage(websocket.BinaryMessage, []byte{MsgPong})
			}
		}
	}()

	// Wait for PTY to finish or connection to close
	select {
	case <-done:
	case <-p.Done():
	}

	// Keep connection alive briefly for final output
	time.Sleep(100 * time.Millisecond)
}
