package broker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestConnAutoApprovesRequestUserInput(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)

		// turn/start → reply
		ts := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": ts["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-1"}}})

		// Server sends an approval request mid-turn.
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"id":      999,
			"method":  "item/tool/requestUserInput",
			"params":  map[string]any{"toolName": "read_file"},
		})
		// Expect the broker to reply with {"answers":{}} (the schema-correct payload).
		approval := readFrame(t, ctx, c)
		if approval["id"] != float64(999) {
			t.Errorf("approval reply id=%v want 999", approval["id"])
		}
		result, ok := approval["result"].(map[string]any)
		if !ok {
			t.Fatalf("approval reply missing result object: %v", approval)
		}
		answers, ok := result["answers"].(map[string]any)
		if !ok {
			t.Errorf("expected result.answers map, got %v", result)
		}
		if len(answers) != 0 {
			t.Errorf("expected empty answers map, got %v", answers)
		}

		// Finish the turn so Turn() returns.
		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/completed",
			"params":  map[string]any{"threadId": "thr-1", "turn": map[string]any{"id": "trn-1", "status": "completed", "items": []any{}, "itemsView": "full", "error": nil}},
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
	if _, err := conn.Turn(ctx, "thr-1", json.RawMessage(`{"input":[{"type":"text","text":"hi"}]}`), 5*time.Second); err != nil {
		t.Fatalf("Turn: %v", err)
	}
}

func TestConnAutoApprovesPermissionsWithEmptyProfile(t *testing.T) {
	url, stop := fakeCodexServer(t, func(t *testing.T, ctx context.Context, c *websocket.Conn) {
		replayHandshake(t, ctx, c)
		ts := readFrame(t, ctx, c)
		writeJSON(t, ctx, c, map[string]any{"jsonrpc": "2.0", "id": ts["id"], "result": map[string]any{"turn": map[string]any{"id": "trn-2"}}})

		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0", "id": 555,
			"method": "item/permissions/requestApproval",
			"params": map[string]any{},
		})
		reply := readFrame(t, ctx, c)
		result, ok := reply["result"].(map[string]any)
		if !ok {
			t.Fatalf("perms reply missing result object: %v", reply)
		}
		perms, ok := result["permissions"].(map[string]any)
		if !ok {
			t.Errorf("expected result.permissions object, got %v", result)
		}
		if len(perms) != 0 {
			t.Errorf("expected empty permissions object, got %v", perms)
		}

		writeJSON(t, ctx, c, map[string]any{
			"jsonrpc": "2.0",
			"method":  "turn/completed",
			"params":  map[string]any{"threadId": "thr-2", "turn": map[string]any{"id": "trn-2", "status": "completed", "items": []any{}, "itemsView": "full", "error": nil}},
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
	if _, err := conn.Turn(ctx, "thr-2", json.RawMessage(`{"input":[]}`), 5*time.Second); err != nil {
		t.Fatalf("Turn: %v", err)
	}
}
