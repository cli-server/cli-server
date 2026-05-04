package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/db"
)

// proxyRouter wires the 3 proxy routes for unit/integration tests.
func proxyRouter(s *Server) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/api/agents/sessions/{sid}/turns/{tid}/cancel", s.handleCancelTurn)
	r.Post("/api/agents/sessions/{sid}/permissions/{pid}", s.handlePermissionDecision)
	r.Get("/api/agents/executors/{id}/status", s.handleExecutorStatus)
	return r
}

// --- Unit tests (no DB needed) ---

func TestCancelTurn_NoAuth_Returns401(t *testing.T) {
	s := &Server{}
	r := proxyRouter(s)
	req := httptest.NewRequest("POST", "/api/agents/sessions/cse_x/turns/trn_y/cancel", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d want 401", rr.Code)
	}
}

func TestCancelTurn_NoCCBroker_Returns503(t *testing.T) {
	s := &Server{} // CCBrokerURL == ""
	r := proxyRouter(s)
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/cse_x/turns/trn_y/cancel", "")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d want 503", rr.Code)
	}
}

func TestCancelTurn_ProxiesToCCBroker(t *testing.T) {
	called := false
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != "POST" {
			t.Errorf("method %s want POST", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cc.Close()

	s := &Server{CCBrokerURL: cc.URL}
	r := proxyRouter(s)
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/cse_x/turns/trn_y/cancel", "")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("status %d want 202", rr.Code)
	}
	if !called {
		t.Error("cc-broker was not called")
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["cancelled"] != true {
		t.Errorf("cancelled=%v want true", resp["cancelled"])
	}
}

func TestExecutorStatus_NoAuth_Returns401(t *testing.T) {
	s := &Server{}
	r := proxyRouter(s)
	req := httptest.NewRequest("GET", "/api/agents/executors/exe_a/status", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d want 401", rr.Code)
	}
}

func TestExecutorStatus_NoRegistry_Returns503(t *testing.T) {
	s := &Server{} // ExecutorRegistryURL == ""
	r := proxyRouter(s)
	req := mustAuthRequest(t, "GET", "/api/agents/executors/exe_a/status", "")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d want 503", rr.Code)
	}
}

func TestExecutorStatus_StreamsUpstreamResponse(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"exe_a","status":"online"}`))
	}))
	defer registry.Close()

	s := &Server{ExecutorRegistryURL: registry.URL}
	r := proxyRouter(s)
	req := mustAuthRequest(t, "GET", "/api/agents/executors/exe_a/status", "")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status %d want 200", rr.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["id"] != "exe_a" {
		t.Errorf("id=%v want exe_a", resp["id"])
	}
}

// --- Integration tests (require TEST_DATABASE_URL) ---

func TestPermissionDecision_WrongResponder_Returns403(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	sid := "cse_perm_wrong_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:              sid,
		WorkspaceID:     "ws_test",
		ExternalID:      "tui:exe_a:perm_wrong",
		CreatorUserID:   "u_test",
		PermissionMode:  "ask",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	// Attach exe_a as responder.
	if _, err := s.DB.AttachResponder(context.Background(), sid, "exe_a", false); err != nil {
		t.Fatalf("AttachResponder: %v", err)
	}

	// Try to decide with exe_b (wrong responder).
	body := `{"decision":"allow","scope":"once","responder_executor_id":"exe_b"}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/permissions/perm_1", body)
	rr := httptest.NewRecorder()
	proxyRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status %d want 403", rr.Code)
	}
}

func TestPermissionDecision_CorrectResponder_ProxiesToCCBroker(t *testing.T) {
	called := false
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["verdict"] != "allow" {
			t.Errorf("verdict=%q want allow", body["verdict"])
		}
		if body["by"] != "exe_a" {
			t.Errorf("by=%q want exe_a", body["by"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer cc.Close()

	s, cleanup := newTestServerTUI(t, cc.URL)
	defer cleanup()

	sid := "cse_perm_ok_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:              sid,
		WorkspaceID:     "ws_test",
		ExternalID:      "tui:exe_a:perm_ok",
		CreatorUserID:   "u_test",
		PermissionMode:  "ask",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	if _, err := s.DB.AttachResponder(context.Background(), sid, "exe_a", false); err != nil {
		t.Fatalf("AttachResponder: %v", err)
	}

	body := `{"decision":"allow","scope":"once","responder_executor_id":"exe_a"}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/permissions/perm_1", body)
	rr := httptest.NewRecorder()
	proxyRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status %d want 200; body=%s", rr.Code, rr.Body)
	}
	if !called {
		t.Error("cc-broker was not called")
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["accepted"] != true {
		t.Errorf("accepted=%v want true", resp["accepted"])
	}
}

func TestPermissionDecision_CCBrokerConflict_Returns409(t *testing.T) {
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer cc.Close()

	s, cleanup := newTestServerTUI(t, cc.URL)
	defer cleanup()

	sid := "cse_perm_conflict_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:              sid,
		WorkspaceID:     "ws_test",
		ExternalID:      "tui:exe_a:perm_conflict",
		CreatorUserID:   "u_test",
		PermissionMode:  "ask",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	if _, err := s.DB.AttachResponder(context.Background(), sid, "exe_a", false); err != nil {
		t.Fatalf("AttachResponder: %v", err)
	}

	body := `{"decision":"allow","scope":"once","responder_executor_id":"exe_a"}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/permissions/perm_1", body)
	rr := httptest.NewRecorder()
	proxyRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("status %d want 409", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "already_resolved") {
		t.Errorf("body missing 'already_resolved': %s", rr.Body)
	}
}
