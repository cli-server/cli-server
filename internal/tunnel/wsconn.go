package tunnel

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// WSConn wraps a WebSocket connection as a net.Conn interface.
// Inspired by xray-core transport/internet/websocket/connection.go.
//
// Write sends each call as a single BinaryMessage.
// Read transparently iterates over WebSocket messages, caching the
// current message reader between calls (xray-core reader-caching pattern).
type WSConn struct {
	ws     *websocket.Conn
	reader io.Reader
	rmu    sync.Mutex // serializes reads
	wmu    sync.Mutex // serializes writes
	ctx    context.Context
	cancel context.CancelFunc
}

// NewWSConn wraps a websocket.Conn into a net.Conn.
func NewWSConn(ctx context.Context, ws *websocket.Conn) *WSConn {
	ctx, cancel := context.WithCancel(ctx)
	// Remove the default read limit so yamux and large payloads work.
	ws.SetReadLimit(-1)
	return &WSConn{
		ws:     ws,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Read reads from the WebSocket message stream.
// Messages are transparently concatenated: when one message ends (EOF),
// the next message is fetched automatically.
func (c *WSConn) Read(b []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()

	for {
		if c.reader != nil {
			n, err := c.reader.Read(b)
			if err == io.EOF {
				c.reader = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}
		// Fetch next WebSocket message.
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return 0, err
		}
		c.reader = bytes.NewReader(data)
	}
}

// Write sends b as a single WebSocket BinaryMessage.
func (c *WSConn) Write(b []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	if err := c.ws.Write(c.ctx, websocket.MessageBinary, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close sends a WebSocket close frame and releases resources.
func (c *WSConn) Close() error {
	c.cancel()
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

func (c *WSConn) LocalAddr() net.Addr  { return wsAddr{} }
func (c *WSConn) RemoteAddr() net.Addr { return wsAddr{} }

func (c *WSConn) SetDeadline(t time.Time) error      { return nil }
func (c *WSConn) SetReadDeadline(t time.Time) error   { return nil }
func (c *WSConn) SetWriteDeadline(t time.Time) error  { return nil }

// wsAddr satisfies net.Addr for the WebSocket connection.
type wsAddr struct{}

func (wsAddr) Network() string { return "websocket" }
func (wsAddr) String() string  { return "websocket" }
