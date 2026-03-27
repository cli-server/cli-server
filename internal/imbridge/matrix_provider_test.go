package imbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMatrixProviderName(t *testing.T) {
	p := &MatrixProvider{}
	if got := p.Name(); got != "matrix" {
		t.Errorf("Name() = %q, want %q", got, "matrix")
	}
}

func TestMatrixProviderJIDSuffix(t *testing.T) {
	p := &MatrixProvider{}
	if got := p.JIDSuffix(); got != "@matrix" {
		t.Errorf("JIDSuffix() = %q, want %q", got, "@matrix")
	}
}

func TestMatrixProviderPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/_matrix/client/v3/sync":
			syncResp := map[string]interface{}{
				"next_batch": "s2_token",
				"rooms": map[string]interface{}{
					"join": map[string]interface{}{
						"!roomA:example.com": map[string]interface{}{
							"timeline": map[string]interface{}{
								"events": []map[string]interface{}{
									{
										"type":             "m.room.message",
										"event_id":         "$evt1",
										"sender":           "@alice:example.com",
										"origin_server_ts": 1700000000000,
										"content": map[string]interface{}{
											"msgtype": "m.text",
											"body":    "Hello from Matrix!",
										},
									},
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(syncResp)

		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := &MatrixProvider{}
	creds := &Credentials{
		BotID:    "@bot:example.com",
		BotToken: "test-token",
		BaseURL:  srv.URL,
	}

	result, err := p.Poll(context.Background(), creds, "s1_token")
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	if result.NewCursor != "s2_token" {
		t.Errorf("NewCursor = %q, want %q", result.NewCursor, "s2_token")
	}

	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}

	msg := result.Messages[0]
	if msg.FromUserID != "!roomA:example.com@matrix" {
		t.Errorf("FromUserID = %q, want %q", msg.FromUserID, "!roomA:example.com@matrix")
	}
	if msg.SenderName != "@alice:example.com" {
		t.Errorf("SenderName = %q, want %q", msg.SenderName, "@alice:example.com")
	}
	if msg.Text != "Hello from Matrix!" {
		t.Errorf("Text = %q, want %q", msg.Text, "Hello from Matrix!")
	}
	if !msg.IsGroup {
		t.Error("expected IsGroup to be true")
	}
	if msg.Metadata["room_id"] != "!roomA:example.com" {
		t.Errorf("Metadata[room_id] = %q, want %q", msg.Metadata["room_id"], "!roomA:example.com")
	}
	if msg.Metadata["event_id"] != "$evt1" {
		t.Errorf("Metadata[event_id] = %q, want %q", msg.Metadata["event_id"], "$evt1")
	}
}

func TestMatrixProviderPollInitial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/_matrix/client/v3/sync":
			// Verify initial sync has timeout=0.
			timeout := r.URL.Query().Get("timeout")
			if timeout != "0" {
				t.Errorf("expected timeout=0 for initial sync, got %s", timeout)
			}

			syncResp := map[string]interface{}{
				"next_batch": "s1_initial",
				"rooms": map[string]interface{}{
					"join": map[string]interface{}{
						"!roomA:example.com": map[string]interface{}{
							"timeline": map[string]interface{}{
								"events": []map[string]interface{}{
									{
										"type":             "m.room.message",
										"event_id":         "$historical1",
										"sender":           "@alice:example.com",
										"origin_server_ts": 1700000000000,
										"content": map[string]interface{}{
											"msgtype": "m.text",
											"body":    "Old message",
										},
									},
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(syncResp)

		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := &MatrixProvider{}
	creds := &Credentials{
		BotID:    "@bot:example.com",
		BotToken: "test-token",
		BaseURL:  srv.URL,
	}

	// Initial poll with empty cursor.
	result, err := p.Poll(context.Background(), creds, "")
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}

	if result.NewCursor != "s1_initial" {
		t.Errorf("NewCursor = %q, want %q", result.NewCursor, "s1_initial")
	}

	// Initial sync should skip all messages.
	if len(result.Messages) != 0 {
		t.Errorf("expected 0 messages from initial sync, got %d", len(result.Messages))
	}
}

func TestMatrixProviderSend(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// The send path should use the room ID with the @matrix suffix stripped.
		if strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:example.com/send/m.room.message/") {
			if r.Method != http.MethodPut {
				t.Errorf("expected PUT, got %s", r.Method)
			}

			body, _ := io.ReadAll(r.Body)
			var content map[string]interface{}
			if err := json.Unmarshal(body, &content); err != nil {
				t.Fatalf("failed to unmarshal request body: %v", err)
			}
			if content["msgtype"] != "m.text" {
				t.Errorf("expected msgtype m.text, got %v", content["msgtype"])
			}
			if content["body"] != "Reply from bot" {
				t.Errorf("expected body 'Reply from bot', got %v", content["body"])
			}

			called = true
			json.NewEncoder(w).Encode(map[string]string{
				"event_id": "$reply1",
			})
			return
		}

		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	p := &MatrixProvider{}
	creds := &Credentials{
		BotToken: "test-token",
		BaseURL:  srv.URL,
	}

	// Send with toUserID that has @matrix suffix (as it would come from Poll).
	err := p.Send(context.Background(), creds, "!room:example.com@matrix", "Reply from bot", nil)
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if !called {
		t.Error("expected send endpoint to be called")
	}
}
