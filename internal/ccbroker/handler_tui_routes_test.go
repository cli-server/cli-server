package ccbroker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/ccbroker/tools"
)

// helper: build a minimal Server with gate + registries wired
func newRoutesTestServer(t *testing.T) *Server {
	s := &Server{
		sse:          NewSSEBroker(),
		activeTurns:  newActiveTurnRegistry(),
		compactQueue: newCompactQueue(),
	}
	s.gate = tools.NewGate(func(_ string, _ tools.Event) {})
	return s
}

func TestDecidePermission_HappyPath(t *testing.T) {
	// Use the server with real SSE-wired gate so permission events reach the
	// SSE broker; this lets us capture the generated permission_id without
	// needing AddPendingForTest from outside the tools package.
	s := newFakeServerWithRealGate(t)

	var capturedPID string
	sub := s.sse.Subscribe("s1")
	defer s.sse.Unsubscribe("s1", sub)

	go func() {
		_ = s.gate.Check(context.Background(), tools.CheckRequest{
			SessionID:            "s1",
			TurnID:               "t1",
			Tool:                 "remote_bash",
			ExecutorID:           "exe",
			Args:                 json.RawMessage(`{}`),
			PermissionMode:       "ask",
			SessionCreatorUserID: "u",
			ExecutorOwnerUserID:  "u",
			Timeout:              5 * time.Second,
		})
	}()

	// Wait for the permission_request SSE event to capture the permission_id.
	select {
	case ev := <-sub.Ch:
		var payload map[string]any
		json.Unmarshal(ev.Payload, &payload)
		capturedPID, _ = payload["permission_id"].(string)
	case <-time.After(time.Second):
		t.Fatal("no permission_request event within 1s")
	}

	body := `{"verdict":"allow","scope":"once"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST",
		"/api/sessions/s1/permissions/"+capturedPID+"/decide",
		strings.NewReader(body))
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Errorf("status %d body=%s", rr.Code, rr.Body)
	}
}

func TestDecidePermission_AlreadyResolved(t *testing.T) {
	s := newRoutesTestServer(t)
	body := `{"verdict":"allow","scope":"once"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST",
		"/api/sessions/s1/permissions/perm_unknown/decide",
		strings.NewReader(body))
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status %d want 409", rr.Code)
	}
}

func TestCancelTurn_Active(t *testing.T) {
	s := newRoutesTestServer(t)
	store := newFakeStore()
	s.store = store
	store.sessions["s1"] = &Session{ID: "s1", WorkspaceID: "ws"}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "t1", SessionID: "s1", WorkspaceID: "ws", UserEventID: "e", UserMessage: "x",
	})
	_ = store.MarkTurnRunning(context.Background(), "t1")
	cancelled := false
	s.activeTurns.Set("s1", "t1", func() { cancelled = true })

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/s1/turns/t1/cancel", nil)
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Errorf("status %d body=%s", rr.Code, rr.Body)
	}
	if !cancelled {
		t.Errorf("cancel func not invoked")
	}
	if !strings.Contains(rr.Body.String(), `"was":"running"`) {
		t.Errorf("body %s should report was:running", rr.Body)
	}
}

func TestCancelTurn_UnknownTurnReturns404(t *testing.T) {
	s := newRoutesTestServer(t)
	store := newFakeStore()
	s.store = store
	cancelled := false
	s.activeTurns.Set("s1", "t1", func() { cancelled = true })

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/s1/turns/t_DIFFERENT/cancel", nil)
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status %d want 404", rr.Code)
	}
	if cancelled {
		t.Errorf("cancel func should NOT be invoked for unknown tid")
	}
}

func TestCancelQueuedTurn(t *testing.T) {
	store := newFakeStore()
	sse := NewSSEBroker()
	srv := &Server{
		store: store, sse: sse,
		activeTurns: newActiveTurnRegistry(),
		gate:        tools.NewGate(func(string, tools.Event) {}),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	store.sessions["sess_q"] = &Session{ID: "sess_q", WorkspaceID: "ws"}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_q", SessionID: "sess_q", WorkspaceID: "ws", UserEventID: "e", UserMessage: "x",
	})

	r := chi.NewRouter()
	r.Post("/api/sessions/{sid}/turns/{tid}/cancel", srv.handleCancelTurn)
	req := httptest.NewRequest("POST", "/api/sessions/sess_q/turns/trn_q/cancel", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("expected 200/202, got %d", rec.Code)
	}
	got, _ := store.GetTurn(context.Background(), "trn_q")
	if got.State != "cancelled" {
		t.Fatalf("expected cancelled, got %s", got.State)
	}
}

func TestCancelTerminalTurnReturns410(t *testing.T) {
	store := newFakeStore()
	srv := &Server{
		store: store, sse: NewSSEBroker(),
		activeTurns: newActiveTurnRegistry(),
		gate:        tools.NewGate(func(string, tools.Event) {}),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_ = store.EnqueueTurn(context.Background(), AgentTurn{
		ID: "trn_t", SessionID: "sess_t", WorkspaceID: "ws", UserEventID: "e", UserMessage: "x",
	})
	_ = store.MarkTurnDone(context.Background(), "trn_t")

	r := chi.NewRouter()
	r.Post("/api/sessions/{sid}/turns/{tid}/cancel", srv.handleCancelTurn)
	req := httptest.NewRequest("POST", "/api/sessions/sess_t/turns/trn_t/cancel", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", rec.Code)
	}
}

func TestGetActiveTurn_ReportsCurrent(t *testing.T) {
	s := newRoutesTestServer(t)
	s.activeTurns.Set("s1", "t99", func() {})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/sessions/s1/turns/active", nil)
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Errorf("status %d", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["turn_id"] != "t99" {
		t.Errorf("turn_id=%v want t99", resp["turn_id"])
	}
}

func TestGetActiveTurn_NoActive_ReturnsNull(t *testing.T) {
	s := newRoutesTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/sessions/s_unknown/turns/active", nil)
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Errorf("status %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"turn_id":null`) {
		t.Errorf("body %s should contain turn_id:null", rr.Body)
	}
}

func TestCompactNow_QueuesForSession(t *testing.T) {
	s := newRoutesTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/s1/compact", nil)
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Errorf("status %d", rr.Code)
	}
	if !s.compactQueue.IsSet("s1") {
		t.Errorf("compactQueue should mark s1")
	}
}

func TestActiveTurnRegistry_RaceSafe(t *testing.T) {
	r := newActiveTurnRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			r.Set("s", "t", func() {})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = r.Get("s")
		}()
	}
	wg.Wait()
}
