package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// countingCodexServer counts how many ws Accepts happen so we can
// verify pool reuse vs fresh dial.
func countingCodexServer(t *testing.T) (urlFn func(workspaceID string) string, dialCount *atomic.Int64, stop func()) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		init := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": init["id"], "result": map[string]any{}})
		readFrame(t, ctx, c) // initialized
		for {
			f, err := readNoFatal(ctx, c)
			if err != nil {
				return
			}
			if f["method"] == "turn/start" {
				writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": f["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-pool"}}})
				writeJSON(t, ctx, c, map[string]any{
					"jsonrpc": "2.0",
					"method":  "turn/completed",
					"params":  map[string]any{"threadId": "thr-x", "turn": map[string]any{"id": "trn-pool", "status": "completed", "items": []any{}, "itemsView": "full", "error": nil}},
				})
			}
		}
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	return func(string) string { return url }, &count, srv.Close
}

func readNoFatal(ctx context.Context, c *websocket.Conn) (map[string]any, error) {
	_, data, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func TestPoolReusesConnForSameWorkspace(t *testing.T) {
	urlFn, dialCount, stop := countingCodexServer(t)
	defer stop()

	resolver := func(ctx context.Context, workspaceID string) (string, error) {
		return urlFn(workspaceID), nil
	}
	p := NewPool(resolver, 5*time.Minute)
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		conn, err := p.Get(ctx, "ws-A")
		if err != nil {
			t.Fatalf("iter %d Get: %v", i, err)
		}
		if _, err := conn.Turn(ctx, "thr-x", json.RawMessage(`{"input":[]}`), 5*time.Second); err != nil {
			t.Fatalf("iter %d Turn: %v", i, err)
		}
	}
	if dialCount.Load() != 1 {
		t.Errorf("dialCount=%d want 1 (pool should reuse)", dialCount.Load())
	}
}

func TestPoolCloseIsIdempotent(t *testing.T) {
	resolver := func(_ context.Context, _ string) (string, error) {
		return "ws://nowhere.invalid", nil
	}
	p := NewPool(resolver, time.Hour)
	// Two consecutive closes must not panic.
	p.Close()
	p.Close()
}

func TestPoolReapsIdleConn(t *testing.T) {
	urlFn, dialCount, stop := countingCodexServer(t)
	defer stop()

	resolver := func(ctx context.Context, _ string) (string, error) { return urlFn(""), nil }
	p := NewPool(resolver, 100*time.Millisecond)
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := p.Get(ctx, "ws-A")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := conn.Turn(ctx, "thr-x", json.RawMessage(`{"input":[]}`), 5*time.Second); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	// Wait past the idle TTL plus the reaper's tick interval.
	time.Sleep(400 * time.Millisecond)

	conn2, err := p.Get(ctx, "ws-A")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if conn2 == conn {
		t.Error("expected fresh Conn after idle reap")
	}
	if dialCount.Load() != 2 {
		t.Errorf("dialCount=%d want 2 (idle reap should force redial)", dialCount.Load())
	}
}
