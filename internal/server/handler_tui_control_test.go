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

// controlRouter wires only the control route for unit/integration tests.
func controlRouter(s *Server) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/api/agents/sessions/{sid}/control", s.handleAgentSessionControl)
	return r
}

// --- Unit tests (no DB needed) ---

func TestControl_NoAuth_Returns401(t *testing.T) {
	s := &Server{}
	r := controlRouter(s)

	req := httptest.NewRequest("POST", "/api/agents/sessions/cse_x/control",
		strings.NewReader(`{"command":"model","args":{"model":"claude-3-5-sonnet"}}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d want 401", rr.Code)
	}
}

func TestControl_UnknownCommand(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	sid := "cse_ctrl_unknown_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_unknown",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control",
		`{"command":"does_not_exist"}`)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d want 400; body=%s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "unknown_command") {
		t.Errorf("body missing unknown_command: %s", rr.Body)
	}
}

// --- Integration tests (require TEST_DATABASE_URL) ---

func TestControl_ModelWritesPreference(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	sid := "cse_ctrl_model_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_model",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	body := `{"command":"model","args":{"model":"claude-opus-4-5"}}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control", body)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200; body=%s", rr.Code, rr.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["applied"] != true {
		t.Errorf("applied=%v want true", resp["applied"])
	}
	if resp["model"] != "claude-opus-4-5" {
		t.Errorf("model=%v want claude-opus-4-5", resp["model"])
	}

	// Verify DB was updated.
	sess, err := s.DB.GetAgentSession(sid)
	if err != nil || sess == nil {
		t.Fatalf("GetAgentSession: %v", err)
	}
	if sess.PreferredModel == nil || *sess.PreferredModel != "claude-opus-4-5" {
		t.Errorf("DB preferred_model=%v want claude-opus-4-5", sess.PreferredModel)
	}
}

func TestControl_ModelMissingArg(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	sid := "cse_ctrl_model_miss_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_model_miss",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control",
		`{"command":"model","args":{}}`)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d want 400; body=%s", rr.Code, rr.Body)
	}
}

func TestControl_PermissionWritesMode(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	sid := "cse_ctrl_perm_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_perm",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	body := `{"command":"permission","args":{"mode":"bypass"}}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control", body)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200; body=%s", rr.Code, rr.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["applied"] != true {
		t.Errorf("applied=%v want true", resp["applied"])
	}
	if resp["mode"] != "bypass" {
		t.Errorf("mode=%v want bypass", resp["mode"])
	}

	// Verify DB was updated.
	sess, err := s.DB.GetAgentSession(sid)
	if err != nil || sess == nil {
		t.Fatalf("GetAgentSession: %v", err)
	}
	if sess.PermissionMode != "bypass" {
		t.Errorf("DB permission_mode=%q want bypass", sess.PermissionMode)
	}
}

func TestControl_PermissionRejectsBadMode(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	sid := "cse_ctrl_perm_bad_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_perm_bad",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	body := `{"command":"permission","args":{"mode":"superuser"}}`
	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control", body)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d want 400; body=%s", rr.Code, rr.Body)
	}
}

func TestControl_CompactProxiesToCCBroker(t *testing.T) {
	// Fake cc-broker that records the compact call.
	called := make(chan string, 1)
	fakeBroker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeBroker.Close()

	s, cleanup := newTestServerTUI(t, fakeBroker.URL)
	defer cleanup()

	sid := "cse_ctrl_compact_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_compact",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control",
		`{"command":"compact"}`)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200; body=%s", rr.Code, rr.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["queued"] != true {
		t.Errorf("queued=%v want true", resp["queued"])
	}

	// Verify cc-broker received the compact call.
	gotPath := <-called
	wantPath := "/api/sessions/" + sid + "/compact"
	if gotPath != wantPath {
		t.Errorf("cc-broker path=%q want %q", gotPath, wantPath)
	}
}

func TestControl_CompactNoCCBroker_Returns503(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "") // no CCBrokerURL
	defer cleanup()

	sid := "cse_ctrl_compact_no503_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_compact_no503",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control", `{"command":"compact"}`)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d want 503", rr.Code)
	}
}

func TestControl_CostReturnsZeroForEmptySession(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()

	sid := "cse_ctrl_cost_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_cost",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control", `{"command":"cost"}`)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200; body=%s", rr.Code, rr.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Both should be 0 (or present as float64 0).
	if resp["input_tokens"] != float64(0) {
		t.Errorf("input_tokens=%v want 0", resp["input_tokens"])
	}
	if resp["output_tokens"] != float64(0) {
		t.Errorf("output_tokens=%v want 0", resp["output_tokens"])
	}
}

func TestControl_AgentsProxiesToRegistry(t *testing.T) {
	// Fake executor-registry.
	fakePayload := `[{"id":"exe_a","workspace_id":"ws_ctrl_test"}]`
	called := make(chan string, 1)
	fakeRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fakePayload))
	}))
	defer fakeRegistry.Close()

	s, cleanup := newTestServerTUI(t, "")
	defer cleanup()
	s.ExecutorRegistryURL = fakeRegistry.URL

	sid := "cse_ctrl_agents_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_agents",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control", `{"command":"agents"}`)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200; body=%s", rr.Code, rr.Body)
	}

	if rr.Body.String() != fakePayload {
		t.Errorf("body=%q want %q", rr.Body.String(), fakePayload)
	}

	// Verify query string.
	gotURI := <-called
	if !strings.Contains(gotURI, "workspace_id=ws_ctrl_test") {
		t.Errorf("registry URI=%q missing workspace_id=ws_ctrl_test", gotURI)
	}
}

func TestControl_AgentsNoRegistry_Returns503(t *testing.T) {
	s, cleanup := newTestServerTUI(t, "") // no ExecutorRegistryURL
	defer cleanup()

	sid := "cse_ctrl_agents_no503_" + t.Name()
	if err := s.DB.CreateAgentSessionTUI(context.Background(), db.CreateTUISessionParams{
		ID:                  sid,
		WorkspaceID:         "ws_ctrl_test",
		ExternalID:          "tui:exe_a:ctrl_agents_no503",
		CreatorUserID:       "u_test",
		PermissionMode:      "ask",
		PreferredExecutorID: "exe_a",
	}); err != nil {
		t.Fatalf("CreateAgentSessionTUI: %v", err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM agent_sessions WHERE id=$1`, sid) })

	req := mustAuthRequest(t, "POST", "/api/agents/sessions/"+sid+"/control", `{"command":"agents"}`)
	rr := httptest.NewRecorder()
	controlRouter(s).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d want 503", rr.Code)
	}
}
