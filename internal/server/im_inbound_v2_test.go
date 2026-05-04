package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agentserver/agentserver/internal/db"
)

// TestProcessWithCCBroker_UsesV2Flow asserts that processWithCCBroker:
//  1. POSTs to /api/v2/turns (not /api/turns)
//  2. Opens GET /api/turns/{tid}/events
//  3. Extracts the final assistant text from the events stream
//  4. Calls IM bridge with that text
func TestProcessWithCCBroker_UsesV2Flow(t *testing.T) {
	var (
		v2Hits     atomic.Int32
		eventsHits atomic.Int32
		mu         sync.Mutex
		seenURLs   []string
	)
	ccbroker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenURLs = append(seenURLs, r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v2/turns":
			v2Hits.Add(1)
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			if req["session_id"] != "sess_im" || req["user_message"] != "ping" {
				t.Errorf("unexpected v2 body: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintln(w, `{"turn_id":"trn_im_test","events_url":"/api/turns/trn_im_test/events"}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/turns/") && strings.HasSuffix(r.URL.Path, "/events"):
			eventsHits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Emit a minimal SSE stream: one assistant message + a result.
			fmt.Fprintf(w, "data: %s\n\n", `{"event_type":"client_event","payload":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello back"}]}}}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"event_type":"client_event","payload":{"type":"result","subtype":"success","is_error":false,"result":"hello back"}}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"event_type":"turn_done","turn_id":"trn_im_test"}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"event_type":"done","turn_id":"trn_im_test"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer ccbroker.Close()

	// Stub IM bridge to record the reply call.
	var imReplyBody []byte
	imbridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		imReplyBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer imbridge.Close()

	srv := &Server{
		CCBrokerURL: ccbroker.URL,
		IMBridgeURL: imbridge.URL,
	}
	chanID := "chan_42"
	session := &db.AgentSession{
		ID:          "sess_im",
		WorkspaceID: "ws_im",
		IMChannelID: &chanID, // matches msg.ChannelID so SetSessionIMChannel branch is skipped
	}
	msg := IMInboundMessage{
		ChatJID:    "user_42",
		SenderName: "Alice",
		Content:    "ping",
		Provider:   "wechat",
		ChannelID:  "chan_42",
	}
	srv.processWithCCBroker(context.Background(), session, msg)

	if v2Hits.Load() != 1 {
		t.Errorf("expected 1 v2 POST, got %d (urls=%v)", v2Hits.Load(), seenURLs)
	}
	if eventsHits.Load() != 1 {
		t.Errorf("expected 1 events GET, got %d (urls=%v)", eventsHits.Load(), seenURLs)
	}
	if imReplyBody == nil {
		t.Fatal("imbridge was never called")
	}
	var reply map[string]string
	_ = json.Unmarshal(imReplyBody, &reply)
	if reply["text"] != "hello back" {
		t.Errorf("expected reply text 'hello back', got %q", reply["text"])
	}
}
