package broker

import (
	"context"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestConnStartThread(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		req := readFrame(t, ctx, c)
		if req["method"] != "thread/start" {
			t.Fatalf("method=%v want thread/start", req["method"])
		}
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result": map[string]any{
				"thread":         map[string]any{"id": "thr-new", "sessionId": "sess", "createdAt": 0, "updatedAt": 0},
				"model":          "gpt-x",
				"modelProvider":  "openai",
				"serviceTier":    nil,
				"cwd":            "/tmp/codex",
				"approvalPolicy": "onRequest",
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

	id, err := conn.StartThread(ctx)
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	if id != "thr-new" {
		t.Errorf("thread id=%q want thr-new", id)
	}
}
