package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
)

// sessionRouter wires a chi router with the 3 agent-session routes (no auth middleware).
func sessionRouter(s *Server) *chi.Mux {
	router := chi.NewRouter()
	router.Post("/api/agents/sessions", s.handleCreateAgentSession)
	router.Post("/api/agents/sessions/{sid}/attach", s.handleAttachAgentSession)
	router.Get("/api/agents/sessions", s.handleListAgentSessions)
	return router
}

// mustAuthRequest builds a request with a pre-injected user_id context and sets
// Content-Type when body is non-empty.
func mustAuthRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	ctx := auth.ContextWithUserID(r.Context(), "u_test")
	return r.WithContext(ctx)
}

// --- Unit tests (no DB needed) ---

func TestCreateAgentSession_RequiresWorkspaceID(t *testing.T) {
	s := &Server{}
	router := sessionRouter(s)

	body := `{"executor_id":"exe_a"}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions", body)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d want 400; body=%s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "invalid") {
		t.Errorf("body missing 'invalid': %s", rr.Body)
	}
}

func TestCreateAgentSession_NoUserID_Returns401(t *testing.T) {
	s := &Server{}
	router := sessionRouter(s)

	body := `{"workspace_id":"ws_test","executor_id":"exe_a"}`
	req := httptest.NewRequest("POST", "/api/agents/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d want 401; body=%s", rr.Code, rr.Body)
	}
}

func TestAttachAgentSession_NoUserID_Returns401(t *testing.T) {
	s := &Server{}
	router := sessionRouter(s)

	body := `{"executor_id":"exe_a"}`
	req := httptest.NewRequest("POST", "/api/agents/sessions/cse_test/attach", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d want 401; body=%s", rr.Code, rr.Body)
	}
}

func TestListAgentSessions_RequiresWorkspaceID(t *testing.T) {
	s := &Server{}
	router := sessionRouter(s)

	req := mustAuthRequest(t, "GET", "/api/agents/sessions?channel_type=tui", "")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d want 400; body=%s", rr.Code, rr.Body)
	}
}

// --- Integration tests (require TEST_DATABASE_URL) ---

func TestCreateAgentSession_TUI(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	body := `{"workspace_id":"ws_test","executor_id":"exe_a","permission_mode":"ask"}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions", body)
	rr := httptest.NewRecorder()
	sessionRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d want 201; body=%s", rr.Code, rr.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	sid, _ := resp["session_id"].(string)
	if sid == "" {
		t.Fatal("session_id missing in response")
	}
	if resp["channel_type"] != "tui" {
		t.Errorf("channel_type=%v want tui", resp["channel_type"])
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
}

func TestAttachAgentSession_OperatorMode(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	// Pre-create a session.
	sid := "cse_t15_attach_op_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_test",
		ExternalID:          "tui:exe_a:1",
		Title:               "attach op test",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid)
	})

	// Attach as operator with also_become_preferred.
	body := `{"executor_id":"exe_b","mode":"operator","also_become_preferred":true}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/attach", body)
	rr := httptest.NewRecorder()
	sessionRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200; body=%s", rr.Code, rr.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["permission_responder"] != "exe_b" {
		t.Errorf("permission_responder=%v want exe_b", resp["permission_responder"])
	}

	// Verify DB was updated.
	sess, err := s.DB.GetAgentSession(sid)
	if err != nil || sess == nil {
		t.Fatalf("GetAgentSession: %v", err)
	}
	if sess.PermissionResponder == nil || *sess.PermissionResponder != "exe_b" {
		t.Errorf("DB permission_responder=%v want exe_b", sess.PermissionResponder)
	}
}

func TestAttachAgentSession_ObserverMode_LeavesResponder(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	// Pre-create session with existing responder.
	sid := "cse_t15_attach_obs_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_test",
		ExternalID:          "tui:exe_a:2",
		Title:               "attach obs test",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid)
	})

	// Set initial responder.
	if _, err := s.DB.AttachResponder(context.Background(), sid, "exe_a", true); err != nil {
		t.Fatalf("AttachResponder setup: %v", err)
	}

	// Attach as observer — should not change the responder.
	body := `{"executor_id":"exe_c","mode":"observer"}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/attach", body)
	rr := httptest.NewRecorder()
	sessionRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200; body=%s", rr.Code, rr.Body)
	}

	// Verify DB responder unchanged.
	sess, err := s.DB.GetAgentSession(sid)
	if err != nil || sess == nil {
		t.Fatalf("GetAgentSession: %v", err)
	}
	if sess.PermissionResponder == nil || *sess.PermissionResponder != "exe_a" {
		t.Errorf("DB permission_responder=%v want exe_a (should be unchanged)", sess.PermissionResponder)
	}
}

func TestListAgentSessions_FiltersByExecutor(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	// Create 2 sessions with different executor_ids embedded in external_id.
	sid1 := "cse_t15_list_a_" + t.Name()
	sid2 := "cse_t15_list_b_" + t.Name()

	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid1,
		WorkspaceID:         "ws_list_test",
		ExternalID:          "tui:exe_alpha:100",
		Title:               "list test alpha",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_alpha",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI sid1: %v", err)
	}
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid2,
		WorkspaceID:         "ws_list_test",
		ExternalID:          "tui:exe_beta:200",
		Title:               "list test beta",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_beta",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI sid2: %v", err)
	}
	t.Cleanup(func() {
		s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid1)
		s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid2)
	})

	// List filtering by exe_alpha — should return only sid1.
	req := mustAuthRequest(t, "GET", "/api/agents/sessions?workspace_id=ws_list_test&channel_type=tui&executor_id=exe_alpha", "")
	rr := httptest.NewRecorder()
	sessionRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200; body=%s", rr.Code, rr.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sessions, _ := resp["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for exe_alpha, got %d", len(sessions))
	}
	sess0, _ := sessions[0].(map[string]any)
	if sess0["session_id"] != sid1 {
		t.Errorf("session_id=%v want %s", sess0["session_id"], sid1)
	}
}
