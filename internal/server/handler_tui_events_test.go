package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/bridge"
	"github.com/agentserver/agentserver/internal/db"
)

// newTestServerForEvents wires BridgeHandler in addition to DB.
// Skips if TEST_DATABASE_URL unset.
func newTestServerForEvents(t *testing.T, ccBrokerURL string) (*Server, func()) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	d, err := db.Open(url)
	if err != nil {
		t.Fatal(err)
		return nil, nil
	}
	s := &Server{
		DB:          d,
		CCBrokerURL: ccBrokerURL,
		BridgeHandler: &bridge.Handler{
			SSE: bridge.NewSSEBroker(),
		},
	}
	cleanup := func() { d.Close() }
	return s, cleanup
}

func TestTUIEvents_NoAuth_Returns401(t *testing.T) {
	s := &Server{}
	r := chi.NewRouter()
	r.Get("/api/agents/sessions/{sid}/events", s.handleTUIEventStream)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/agents/sessions/cse_x/events", nil)
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d want 401", rr.Code)
	}
}

func TestTUIEvents_UnknownSession_Returns404(t *testing.T) {
	s, cleanup := newTestServerForEvents(t, "")
	defer cleanup()

	router := chi.NewRouter()
	router.Get("/api/agents/sessions/{sid}/events", s.handleTUIEventStream)

	rr := httptest.NewRecorder()
	req := mustAuthRequest(t, "GET", "/api/agents/sessions/cse_does_not_exist_xyz/events", "")
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status %d want 404", rr.Code)
	}
}

func TestTUIEvents_BridgesCCBrokerSSE(t *testing.T) {
	// Start a fake cc-broker that streams 3 events.
	ccBroker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/turns") {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i, ev := range []string{"tool_use", "tool_result", "turn_done"} {
			fmt.Fprintf(w, "event: %s\ndata: {\"seq\":%d}\n\n", ev, i+1)
			f.Flush()
		}
	}))
	defer ccBroker.Close()

	s, cleanup := newTestServerForEvents(t, ccBroker.URL)
	defer cleanup()

	sid := "cse_evt_bridge_" + strings.ReplaceAll(t.Name(), "/", "_")
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_test",
		ExternalID:          "tui:exe_a:bridge_test",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	defer s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid)
	defer s.DB.Exec(`DELETE FROM agent_session_events WHERE session_id=$1`, sid)

	// Subscribe to SSE in a goroutine.
	received := make(chan string, 8)
	var subWG sync.WaitGroup
	subWG.Add(1)
	go func() {
		defer subWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		req := mustAuthRequest(t, "GET", "/api/agents/sessions/"+sid+"/events", "")
		req = req.WithContext(ctx)
		router := chi.NewRouter()
		router.Get("/api/agents/sessions/{sid}/events", s.handleTUIEventStream)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		for _, line := range strings.Split(rr.Body.String(), "\n") {
			if strings.HasPrefix(line, "event: ") {
				received <- strings.TrimPrefix(line, "event: ")
			}
		}
		close(received)
	}()

	// Let subscriber connect before triggering inbound.
	time.Sleep(150 * time.Millisecond)

	// Trigger inbound which calls cc-broker → bridges events.
	body := `{"executor_id":"exe_a","text":"hi","session_id":"` + sid + `","permission_responder":true}`
	inboundRR := httptest.NewRecorder()
	inboundReq := mustAuthRequest(t, "POST", "/api/agents/workspaces/ws_test/inbound", body)
	inboundRouter := chi.NewRouter()
	inboundRouter.Post("/api/agents/workspaces/{wid}/inbound", s.handleTUIInbound)
	inboundRouter.ServeHTTP(inboundRR, inboundReq)
	if inboundRR.Code != http.StatusAccepted {
		t.Fatalf("inbound %d body=%s", inboundRR.Code, inboundRR.Body)
	}

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
loop:
	for len(seen) < 3 {
		select {
		case ev, ok := <-received:
			if !ok {
				break loop
			}
			seen[ev] = true
		case <-deadline:
			break loop
		}
	}

	for _, want := range []string{"tool_use", "tool_result", "turn_done"} {
		if !seen[want] {
			t.Errorf("missing event %q (seen=%v)", want, seen)
		}
	}
	subWG.Wait()
}
