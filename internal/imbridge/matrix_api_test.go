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

func TestMatrixWhoami(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/account/whoami" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token-123" {
			t.Errorf("unexpected auth header: %s", auth)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"user_id":   "@bot:example.com",
			"device_id": "ABCDEF",
		})
	}))
	defer srv.Close()

	userID, err := MatrixWhoami(context.Background(), srv.URL, "test-token-123")
	if err != nil {
		t.Fatalf("MatrixWhoami returned error: %v", err)
	}
	if userID != "@bot:example.com" {
		t.Errorf("expected user_id @bot:example.com, got %s", userID)
	}
}

func TestMatrixSendText(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The path should be: /_matrix/client/v3/rooms/!room:example.com/send/m.room.message/{txnID}
		if !strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:example.com/send/m.room.message/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
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
		if content["body"] != "Hello, Matrix!" {
			t.Errorf("expected body 'Hello, Matrix!', got %v", content["body"])
		}

		called = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"event_id": "$event123",
		})
	}))
	defer srv.Close()

	err := MatrixSendText(context.Background(), srv.URL, "test-token", "!room:example.com", "Hello, Matrix!")
	if err != nil {
		t.Fatalf("MatrixSendText returned error: %v", err)
	}
	if !called {
		t.Error("expected send endpoint to be called")
	}
}

func TestMatrixSync(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/_matrix/client/v3/sync":
			requestCount++
			// Verify the since and timeout query params.
			since := r.URL.Query().Get("since")
			if since != "s1_token" {
				t.Errorf("expected since=s1_token, got %s", since)
			}

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
											"body":    "Hi there!",
										},
									},
									{
										// This message is from the bot itself and should be filtered out.
										"type":             "m.room.message",
										"event_id":         "$evt2",
										"sender":           "@bot:example.com",
										"origin_server_ts": 1700000001000,
										"content": map[string]interface{}{
											"msgtype": "m.text",
											"body":    "I am a bot",
										},
									},
									{
										// This is a state event and should be filtered out.
										"type":      "m.room.member",
										"event_id":  "$evt3",
										"sender":    "@carol:example.com",
										"state_key": "@carol:example.com",
										"content": map[string]interface{}{
											"membership": "join",
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

	messages, nextBatch, err := MatrixSync(context.Background(), srv.URL, "test-token", "@bot:example.com", "s1_token", 1)
	if err != nil {
		t.Fatalf("MatrixSync returned error: %v", err)
	}
	if nextBatch != "s2_token" {
		t.Errorf("expected next_batch=s2_token, got %s", nextBatch)
	}

	// Should only return Alice's message (bot's own message and state event filtered out).
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	msg := messages[0]
	if msg.RoomID != "!roomA:example.com" {
		t.Errorf("expected roomID=!roomA:example.com, got %s", msg.RoomID)
	}
	if msg.EventID != "$evt1" {
		t.Errorf("expected eventID=$evt1, got %s", msg.EventID)
	}
	if msg.SenderID != "@alice:example.com" {
		t.Errorf("expected senderID=@alice:example.com, got %s", msg.SenderID)
	}
	if msg.Text != "Hi there!" {
		t.Errorf("expected text='Hi there!', got %s", msg.Text)
	}
	if msg.Timestamp != 1700000000000 {
		t.Errorf("expected timestamp=1700000000000, got %d", msg.Timestamp)
	}
}
