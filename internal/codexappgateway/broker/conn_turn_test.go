package broker

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// replayHandshake reads initialize + initialized frames and replies to
// initialize so Dial completes. Returns once both are seen.
func replayHandshake(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	init := readFrame(t, ctx, c)
	writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": init["id"], "result": map[string]any{}})
	got := readFrame(t, ctx, c)
	if got["method"] != "initialized" {
		t.Fatalf("expected initialized, got %v", got)
	}
}

func TestConnTurnSuccessful(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)

		// Expect turn/start call.
		ts := readFrame(t, ctx, c)
		if ts["method"] != "turn/start" {
			t.Fatalf("want turn/start, got %v", ts["method"])
		}
		params := ts["params"].(map[string]any)
		if params["threadId"] != "thr-abc" {
			t.Errorf("threadId=%v", params["threadId"])
		}
		// Reply with turn/start response (turn id).
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"id":      ts["id"],
			"result":  map[string]any{"turn": map[string]any{"id": "trn-001"}},
		})

		// Stream notifications then turn/completed.
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/started",
			"params":  map[string]any{"threadId": "thr-abc", "turn": map[string]any{"id": "trn-001"}},
		})
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/completed",
			"params": map[string]any{
				"threadId": "thr-abc",
				"turn": map[string]any{
					"id":          "trn-001",
					"status":      "completed",
					"itemsView":   "full",
					"items":       []any{map[string]any{"type": "agentMessage", "id": "msg1", "text": "hello"}},
					"error":       nil,
					"startedAt":   1,
					"completedAt": 2,
					"durationMs":  1000,
				},
			},
		})
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	rawTurn, err := conn.Turn(ctx, "thr-abc", json.RawMessage(`{"input":[{"type":"text","text":"hi"}]}`), 5*time.Second)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rawTurn, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "completed" {
		t.Errorf("status=%v", got["status"])
	}
	items := got["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["text"] != "hello" {
		t.Errorf("items=%v", items)
	}
}

// TestReadLoopDoesNotLeakWatcherGoroutines verifies that readLoop spawns
// exactly one watcher goroutine for the connection lifetime, not one per
// received frame.
func TestReadLoopDoesNotLeakWatcherGoroutines(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		// Reply to turn/start so Turn() can proceed.
		ts := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": ts["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-x"}}})
		// Send 50 cheap notifications; each used to spawn a leaked goroutine.
		for i := 0; i < 50; i++ {
			writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "method": "turn/diff/updated", "params": map[string]any{"threadId": "t", "turnId": "trn-x", "diff": ""}})
		}
		// Complete the turn.
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{"threadId": "t", "turn": map[string]any{"id": "trn-x", "status": "completed", "items": []any{}, "itemsView": "full", "error": nil}},
		})
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before := runtime.NumGoroutine()

	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := conn.Turn(ctx, "t", json.RawMessage(`{"input":[]}`), 5*time.Second); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	conn.Close()
	// Give the watcher + reader goroutines time to exit.
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after-before > 2 {
		t.Errorf("goroutine count delta %d (before=%d after=%d) — expected ≤ 2", after-before, before, after)
	}
}

// TestTurnCallerCtxCancelCleansPendingResp verifies that cancelling the
// caller's ctx before turn/start response arrives removes the pendingResp
// entry, preventing a stale map entry from persisting until Close().
func TestTurnCallerCtxCancelCleansPendingResp(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		// Read turn/start but never reply, so caller hits ctx cancellation.
		_ = readFrame(t, ctx, c)
		// Hold the ws open until the test ends.
		<-ctx.Done()
	})
	defer stop()

	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	conn, err := Dial(dctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	cctx, ccancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		ccancel()
	}()
	_, err = conn.Turn(cctx, "t", json.RawMessage(`{"input":[]}`), 5*time.Second)
	if err == nil {
		t.Fatal("expected ctx cancellation error")
	}
	// Verify map is clean.
	conn.mu.Lock()
	n := len(conn.pendingResp)
	conn.mu.Unlock()
	if n != 0 {
		t.Errorf("pendingResp leaked %d entries", n)
	}
}

// TestConnTurnAccumulatesItemsFromItemCompleted verifies that items
// streamed via item/completed notifications during a turn end up
// injected into the final Turn.items list when delivered to Turn().
// Codex v2 sends turn/completed with TurnItemsView::NotLoaded (empty
// items) and emits items separately during the stream; without
// accumulation the REST caller sees no agentMessage.
func TestConnTurnAccumulatesItemsFromItemCompleted(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		ts := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0", "id": ts["id"],
			"result": map[string]any{"turn": map[string]any{"id": "trn-a"}},
		})
		// Stream two item/completed events (reasoning + agentMessage),
		// then turn/completed with EMPTY Turn.items (the v2-faithful shape).
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0", "method": "item/completed",
			"params": map[string]any{
				"threadId": "thr-a", "turnId": "trn-a",
				"item": map[string]any{"type": "reasoning", "id": "r1", "summary": []any{"thinking..."}, "content": []any{}},
			},
		})
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0", "method": "item/completed",
			"params": map[string]any{
				"threadId": "thr-a", "turnId": "trn-a",
				"item": map[string]any{"type": "agentMessage", "id": "m1", "text": "hello"},
			},
		})
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{
				"threadId": "thr-a",
				"turn": map[string]any{
					"id": "trn-a", "status": "completed",
					"items": []any{}, "itemsView": "notLoaded", "error": nil,
				},
			},
		})
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	raw, err := conn.Turn(ctx, "thr-a", json.RawMessage(`{"input":[{"type":"text","text":"hi"}]}`), 5*time.Second)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items, ok := got["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected 2 injected items, got %v", got["items"])
	}
	last := items[len(items)-1].(map[string]any)
	if last["type"] != "agentMessage" || last["text"] != "hello" {
		t.Errorf("last item = %v, want agentMessage/hello", last)
	}
	// Suppress unused-import warning on slow CI re-runs.
	_ = runtime.NumGoroutine()
}
