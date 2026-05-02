package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendMessage_PostsToIMBridge(t *testing.T) {
	var captured map[string]any
	var gotPath string
	var gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSecret = r.Header.Get("X-Internal-Secret")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"sent"}`))
	}))
	defer srv.Close()

	tctx := &Context{
		IMChannelID:       "ch1",
		IMUserID:          "u1",
		IMBridgeURL:       srv.URL,
		InternalAPISecret: "topsecret",
		HTTP:              http.DefaultClient,
	}
	tool := byName(imTools(tctx), "send_message")
	r, _ := tool.Handler(context.Background(),
		json.RawMessage(`{"text":"hello"}`))
	if r.IsError {
		t.Fatalf("IsError: %v", r.Content)
	}
	if gotPath != "/api/internal/imbridge/send" {
		t.Errorf("path=%q want /api/internal/imbridge/send", gotPath)
	}
	if captured["channel_id"] != "ch1" || captured["to_user_id"] != "u1" || captured["text"] != "hello" {
		t.Errorf("unexpected body: %v", captured)
	}
	if gotSecret != "topsecret" {
		t.Errorf("X-Internal-Secret=%q want topsecret", gotSecret)
	}
}

func TestSendMessage_NoIMBridgeURL(t *testing.T) {
	tctx := &Context{
		IMChannelID: "ch1",
		IMUserID:    "u1",
		HTTP:        http.DefaultClient,
		// IMBridgeURL intentionally empty
	}
	tool := byName(imTools(tctx), "send_message")
	r, _ := tool.Handler(context.Background(),
		json.RawMessage(`{"text":"hello"}`))
	if !r.IsError {
		t.Errorf("expected IsError when IMBridgeURL is not configured")
	}
}

func TestSendMessage_EmptyText(t *testing.T) {
	tctx := &Context{
		IMChannelID: "ch1",
		IMUserID:    "u1",
		IMBridgeURL: "http://imbridge.example",
		HTTP:        http.DefaultClient,
	}
	tool := byName(imTools(tctx), "send_message")
	r, _ := tool.Handler(context.Background(),
		json.RawMessage(`{"text":""}`))
	if !r.IsError {
		t.Errorf("expected IsError when text is empty")
	}
}

func TestSendFile_ReturnsError(t *testing.T) {
	tctx := &Context{HTTP: http.DefaultClient}
	tool := byName(imTools(tctx), "send_file")
	r, _ := tool.Handler(context.Background(),
		json.RawMessage(`{"source":"x","filename":"x.txt"}`))
	if !r.IsError {
		t.Errorf("expected IsError for send_file (not yet supported)")
	}
}
