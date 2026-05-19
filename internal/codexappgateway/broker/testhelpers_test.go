package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nhooyr.io/websocket"
)

// fakeCodexServer accepts one ws connection and runs `frame` against it.
// frame receives Read/Write helpers and must replay codex behavior.
//
// After frame returns we block on a best-effort read until the client
// closes the ws or the request context expires. Without this the
// httptest server's defer-close races client-side readLoop dispatch of
// the last queued notification (e.g. turn/completed): the close frame
// arrives at the readLoop before the buffered turn/completed, the
// loop fails all pending channels with "connection closed", and
// Conn.Turn returns a transport error mid-test. Holding the ws open
// until the client side closes makes the test robust on slow CI.
func fakeCodexServer(t *testing.T, frame func(t *testing.T, ctx context.Context, c *websocket.Conn)) (wsURL string, stop func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Logf("accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		frame(t, r.Context(), c)
		// Drain until the client closes — keeps the ws open so any
		// frames written above are delivered before we tear down.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	return url, srv.Close
}

func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func writeJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}
