package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
)

// newTestServerTUI opens a real DB (skips if TEST_DATABASE_URL is unset) and
// returns a *Server wired with the given ccBrokerURL.
func newTestServerTUI(t *testing.T, ccBrokerURL string) (*Server, func()) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	d, err := db.Open(url)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB:          d,
		CCBrokerURL: ccBrokerURL,
	}
	return s, func() { d.Close() }
}

// mustTUIRequest builds a request with a pre-injected user_id context (bypasses
// real auth middleware) and sets Content-Type when body is non-empty.
func mustTUIRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := auth.ContextWithUserID(req.Context(), "u_test")
	return req.WithContext(ctx)
}

// tuiRouter wires a chi router with only the TUI inbound route (no auth middleware).
func tuiRouter(s *Server) *chi.Mux {
	router := chi.NewRouter()
	router.Post("/api/workspaces/{wid}/tui/inbound", s.handleTUIInbound)
	return router
}

// --- Unit tests (no DB needed) ---

func TestTUIInbound_MissingExecutorID(t *testing.T) {
	s := &Server{}
	router := tuiRouter(s)

	body := `{"text":"hello"}`
	req := mustTUIRequest(t, "POST", "/api/workspaces/ws_test/tui/inbound", body)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d want 400", rr.Code)
	}
}

func TestTUIInbound_MissingText(t *testing.T) {
	s := &Server{}
	router := tuiRouter(s)

	body := `{"executor_id":"exe_a"}`
	req := mustTUIRequest(t, "POST", "/api/workspaces/ws_test/tui/inbound", body)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d want 400", rr.Code)
	}
}

func TestTUIInbound_NoUserID_Returns401(t *testing.T) {
	s := &Server{}
	router := tuiRouter(s)

	body := `{"executor_id":"exe_a","text":"hi"}`
	// Request without injected user — auth.UserIDFromContext returns "".
	req := httptest.NewRequest("POST", "/api/workspaces/ws_test/tui/inbound", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d want 401", rr.Code)
	}
}

// --- Integration tests (require TEST_DATABASE_URL) ---

func TestTUIInbound_NewSession_CreatesAndReturns202(t *testing.T) {
	fakeCC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: turn_done\ndata: {}\n\n"))
	}))
	defer fakeCC.Close()

	s, cleanup := newTestServerTUI(t, fakeCC.URL)
	defer cleanup()

	body := `{"executor_id":"exe_a","text":"hello","permission_responder":true,"metadata":{"channel_type":"tui"}}`
	rr := httptest.NewRecorder()
	req := mustTUIRequest(t, "POST", "/api/workspaces/ws_test/tui/inbound", body)
	tuiRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	sid, _ := resp["session_id"].(string)
	turnID, _ := resp["turn_id"].(string)
	if sid == "" {
		t.Error("session_id missing in response")
	}
	if turnID == "" {
		t.Error("turn_id missing in response")
	}

	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid)
	})

	sess, err := s.DB.GetAgentSession(sid)
	if err != nil {
		t.Fatalf("GetAgentSession: %v", err)
	}
	if sess == nil {
		t.Fatal("session not in DB")
	}
	if sess.ChannelType != "tui" {
		t.Errorf("channel_type=%q want tui", sess.ChannelType)
	}
	if sess.PermissionMode != "ask" {
		t.Errorf("permission_mode=%q want ask", sess.PermissionMode)
	}
	if sess.PermissionResponder == nil || *sess.PermissionResponder != "exe_a" {
		t.Errorf("permission_responder=%v want exe_a", sess.PermissionResponder)
	}
}

func TestTUIInbound_TurnInProgress_Returns409(t *testing.T) {
	fakeCC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Intentionally hang so the goroutine is stuck during the second request.
	}))
	defer fakeCC.Close()

	s, cleanup := newTestServerTUI(t, fakeCC.URL)
	defer cleanup()

	sid := "cse_t14_conflict_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_test",
		ExternalID:          "tui:exe_a:1",
		Title:               "conflict test",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid)
	})

	// Pre-claim a turn so the second request sees a conflict.
	if _, err := s.DB.ClaimActiveTurn(context.Background(), sid, "trn_existing"); err != nil {
		t.Fatalf("ClaimActiveTurn: %v", err)
	}

	body := `{"session_id":"` + sid + `","executor_id":"exe_a","text":"hi"}`
	rr := httptest.NewRecorder()
	req := mustTUIRequest(t, "POST", "/api/workspaces/ws_test/tui/inbound", body)
	tuiRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("status %d want 409; body=%s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "turn_in_progress") {
		t.Errorf("body missing 'turn_in_progress': %s", rr.Body)
	}
}

// silence unused imports
var _ = io.Discard
