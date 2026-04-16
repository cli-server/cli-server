package executorregistry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// setupTestServer creates a Store backed by TEST_DATABASE_URL and returns a
// fully-wired Server. The test is skipped when TEST_DATABASE_URL is not set.
// t.Cleanup truncates the executor tables so each test starts with a clean slate.
func setupTestServer(t *testing.T) *Server {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	store, err := NewStore(dbURL)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	t.Cleanup(func() {
		// Remove test data; order matters due to FK constraints.
		store.Exec(`DELETE FROM executor_heartbeats`)
		store.Exec(`DELETE FROM executor_capabilities`)
		store.Exec(`DELETE FROM executors`)
		store.Close()
	})

	cfg := Config{
		Port:     "0",
		LogLevel: 0, // LevelInfo
	}
	return NewServer(cfg, store)
}

// doRequest is a convenience helper that runs a request through the router and
// returns the recorded response.
func doRequest(t *testing.T, srv *Server, method, target string, body any, authToken string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, target, reqBody)
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	return rr
}

// registerAgent is a test helper that registers a local agent and returns the
// parsed registerResponse. It fails the test on any error or unexpected status.
func registerAgent(t *testing.T, srv *Server, name, workspaceID string) registerResponse {
	t.Helper()
	rr := doRequest(t, srv, http.MethodPost, "/api/executors/register",
		map[string]string{"name": name, "workspace_id": workspaceID}, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp registerResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return resp
}

// TestRegisterAndQuery registers a local agent and then verifies it appears in
// the list response with the expected executor_id.
func TestRegisterAndQuery(t *testing.T) {
	srv := setupTestServer(t)
	const wsID = "ws_test_regquery"

	// 1. Register
	resp := registerAgent(t, srv, "test-agent", wsID)

	// 2. Validate registration response fields
	if resp.ExecutorID == "" {
		t.Error("executor_id is empty")
	}
	if resp.TunnelToken == "" {
		t.Error("tunnel_token is empty")
	}
	if resp.RegistryToken == "" {
		t.Error("registry_token is empty")
	}

	// 3. List executors for the workspace
	rr := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/executors?workspace_id=%s", wsID), nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	var executors []ExecutorInfo
	if err := json.NewDecoder(rr.Body).Decode(&executors); err != nil {
		t.Fatalf("decode list response: %v", err)
	}

	// 4. Verify exactly one entry with the matching ID
	if len(executors) != 1 {
		t.Fatalf("list: want 1 executor, got %d", len(executors))
	}
	if executors[0].ID != resp.ExecutorID {
		t.Errorf("list executor ID mismatch: got %q, want %q", executors[0].ID, resp.ExecutorID)
	}
}

// TestHeartbeat registers an agent, sends a valid heartbeat, then verifies that
// an invalid token is rejected with 401.
func TestHeartbeat(t *testing.T) {
	srv := setupTestServer(t)
	const wsID = "ws_test_heartbeat"

	// 1. Register
	reg := registerAgent(t, srv, "heartbeat-agent", wsID)

	// 2. Valid heartbeat
	target := fmt.Sprintf("/api/executors/%s/heartbeat", reg.ExecutorID)
	rr := doRequest(t, srv, http.MethodPut, target,
		heartbeatRequest{Status: "online"}, reg.RegistryToken)
	if rr.Code != http.StatusOK {
		t.Errorf("heartbeat: want 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	// 3. Invalid token → 401
	rr = doRequest(t, srv, http.MethodPut, target,
		heartbeatRequest{Status: "online"}, "wrong-token")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("heartbeat with wrong token: want 401, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestRegisterSandbox registers a sandbox executor with capabilities and verifies
// both the 201 response and that the capabilities description is persisted.
func TestRegisterSandbox(t *testing.T) {
	srv := setupTestServer(t)
	const wsID = "ws_test_sandbox"
	const sandboxID = "sbx_integration_test_001"
	const wantDescription = "test sandbox environment"

	// 1. Register sandbox with capabilities
	reqBody := registerSandboxRequest{
		SandboxID:   sandboxID,
		WorkspaceID: wsID,
		Name:        "test-sandbox",
		Capabilities: &ExecutorCapability{
			Description: wantDescription,
			Tools:       []string{"bash", "read_file"},
		},
	}
	rr := doRequest(t, srv, http.MethodPost, "/api/executors/sandbox", reqBody, "")

	// 2. Assert 201
	if rr.Code != http.StatusCreated {
		t.Fatalf("sandbox register: want 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	// 3. List executors and verify capabilities.description is set
	listRR := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/executors?workspace_id=%s", wsID), nil, "")
	if listRR.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d (body: %s)", listRR.Code, listRR.Body.String())
	}

	var executors []ExecutorInfo
	if err := json.NewDecoder(listRR.Body).Decode(&executors); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(executors) != 1 {
		t.Fatalf("list: want 1 executor, got %d", len(executors))
	}
	if executors[0].Capabilities.Description != wantDescription {
		t.Errorf("capabilities.description: got %q, want %q",
			executors[0].Capabilities.Description, wantDescription)
	}
}

// TestUpdateCapabilities registers an agent, updates its capabilities, then
// fetches the executor and verifies the new capabilities are stored.
func TestUpdateCapabilities(t *testing.T) {
	srv := setupTestServer(t)
	const wsID = "ws_test_caps"

	// 1. Register
	reg := registerAgent(t, srv, "caps-agent", wsID)

	// 2. Update capabilities
	newCaps := ExecutorCapability{
		Description: "updated description",
		Tools:       []string{"write_file", "bash"},
		WorkingDir:  "/workspace",
		Resources: ResourceInfo{
			CPUCores: 4,
			MemoryGB: 8,
		},
	}
	target := fmt.Sprintf("/api/executors/%s/capabilities", reg.ExecutorID)
	rr := doRequest(t, srv, http.MethodPut, target, newCaps, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("update capabilities: want 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	// 3. Fetch executor and verify capabilities
	getTarget := fmt.Sprintf("/api/executors/%s", reg.ExecutorID)
	getRR := doRequest(t, srv, http.MethodGet, getTarget, nil, "")
	if getRR.Code != http.StatusOK {
		t.Fatalf("get executor: want 200, got %d (body: %s)", getRR.Code, getRR.Body.String())
	}

	var info ExecutorInfo
	if err := json.NewDecoder(getRR.Body).Decode(&info); err != nil {
		t.Fatalf("decode executor: %v", err)
	}
	if info.Capabilities.Description != newCaps.Description {
		t.Errorf("description: got %q, want %q",
			info.Capabilities.Description, newCaps.Description)
	}
	if info.Capabilities.WorkingDir != newCaps.WorkingDir {
		t.Errorf("working_dir: got %q, want %q",
			info.Capabilities.WorkingDir, newCaps.WorkingDir)
	}
	if len(info.Capabilities.Tools) != len(newCaps.Tools) {
		t.Errorf("tools length: got %d, want %d",
			len(info.Capabilities.Tools), len(newCaps.Tools))
	}
}
