//go:build integration

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
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/db"
)

// TestE2E_TUITurnFlow walks through:
// 1. POST /tui/inbound creates a session and a turn
// 2. SSE subscriber sees tool_use, permission_request, tool_result, turn_done
// 3. POST /permissions/{pid} resolves the request, cc-broker continues
//
// Fake cc-broker streams the canonical event sequence.
func TestE2E_TUITurnFlow(t *testing.T) {
	decideRcv := make(chan map[string]string, 1)

	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/api/v2/turns"):
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			tid, _ := req["turn_id"].(string)
			if tid == "" {
				tid = "trn_stub_e2e"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"turn_id":%q,"events_url":"/api/turns/%s/events"}`, tid, tid)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/turns/") && strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			fmt.Fprint(w, "event: tool_use\ndata: {\"tool\":\"remote_bash\",\"executor_id\":\"exe_a\"}\n\n")
			f.Flush()
			fmt.Fprint(w, "event: permission_request\ndata: {\"permission_id\":\"perm_e\"}\n\n")
			f.Flush()
			// wait for decide
			select {
			case <-decideRcv:
			case <-time.After(2 * time.Second):
				t.Errorf("decide timeout")
			}
			fmt.Fprint(w, "event: tool_result\ndata: {\"output\":\"ok\"}\n\n")
			f.Flush()
			fmt.Fprint(w, "event: turn_done\ndata: {}\n\n")
			f.Flush()
		case strings.Contains(r.URL.Path, "/permissions/perm_e/decide"):
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			decideRcv <- b
			w.WriteHeader(200)
		}
	}))
	defer cc.Close()

	s, cleanup := newTestServerForEvents(t, cc.URL)
	if s == nil {
		return
	} // skipped
	defer cleanup()

	// Build a session beforehand so we can subscribe SSE before the turn.
	sid := "cse_e2e_" + t.Name()
	_ = s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID: sid, WorkspaceID: "ws_test", ExternalID: "tui:exe_a:1",
		CreatorUserID: "u_test", PermissionMode: "ask", PreferredExecutorID: "exe_a",
	})
	_, _ = s.DB.AttachResponder(context.Background(), sid, "exe_a", true)
	defer s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid)
	defer s.DB.Exec(`DELETE FROM agent_session_events WHERE session_id=$1`, sid)

	received := make(chan string, 16)
	var subWG sync.WaitGroup
	subWG.Add(1)
	go func() {
		defer subWG.Done()
		rr := httptest.NewRecorder()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := mustAuthRequest(t, "GET", "/api/agents/sessions/"+sid+"/events", "")
		req = req.WithContext(ctx)
		router := chi.NewRouter()
		router.Get("/api/agents/sessions/{sid}/events", s.handleTUIEventStream)
		router.ServeHTTP(rr, req)
		for _, line := range strings.Split(rr.Body.String(), "\n") {
			if strings.HasPrefix(line, "event: ") {
				received <- strings.TrimPrefix(line, "event: ")
			}
		}
		close(received)
	}()
	time.Sleep(100 * time.Millisecond)

	// POST inbound (use existing session).
	body := `{"executor_id":"exe_a","text":"hi","session_id":"` + sid + `"}`
	rr := httptest.NewRecorder()
	req := mustAuthRequest(t, "POST", "/api/agents/workspaces/ws_test/inbound", body)
	inbR := chi.NewRouter()
	inbR.Post("/api/agents/workspaces/{wid}/inbound", s.handleTUIInbound)
	inbR.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("inbound status %d body=%s", rr.Code, rr.Body)
	}

	// Wait for permission_request.
	deadline := time.After(3 * time.Second)
	var sawPerm bool
	for !sawPerm {
		select {
		case ev := <-received:
			if ev == "permission_request" {
				sawPerm = true
			}
		case <-deadline:
			t.Fatal("no permission_request observed")
		}
	}

	// POST decide.
	decBody := `{"decision":"allow","scope":"once","responder_executor_id":"exe_a"}`
	decRR := httptest.NewRecorder()
	decReq := mustAuthRequest(t, "POST",
		"/api/agents/sessions/"+sid+"/permissions/perm_e", decBody)
	decR := chi.NewRouter()
	decR.Post("/api/agents/sessions/{sid}/permissions/{pid}", s.handlePermissionDecision)
	decR.ServeHTTP(decRR, decReq)
	if decRR.Code != http.StatusOK {
		t.Errorf("decide %d body=%s", decRR.Code, decRR.Body)
	}

	// Drain remaining events; expect tool_result + turn_done.
	needed := map[string]bool{"tool_result": false, "turn_done": false}
	deadline = time.After(3 * time.Second)
	for !needed["tool_result"] || !needed["turn_done"] {
		select {
		case ev, ok := <-received:
			if !ok {
				goto check
			}
			if _, want := needed[ev]; want {
				needed[ev] = true
			}
		case <-deadline:
			goto check
		}
	}
check:
	for k, v := range needed {
		if !v {
			t.Errorf("missing event %q", k)
		}
	}
	subWG.Wait()
}
