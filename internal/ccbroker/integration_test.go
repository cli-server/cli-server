package ccbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const testJWTSecret = "test-secret-change-in-production-32chars"

// setupTestServer creates a Store backed by TEST_DATABASE_URL and returns a
// fully-wired Server. The test is skipped when TEST_DATABASE_URL is not set.
// t.Cleanup truncates the cc-broker tables so each test starts with a clean slate.
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
		store.Exec(`DELETE FROM agent_session_events`)
		store.Exec(`DELETE FROM agent_session_internal_events`)
		store.Exec(`DELETE FROM agent_session_workers`)
		store.Exec(`DELETE FROM agent_sessions`)
		store.Close()
	})

	cfg := Config{
		Port:      "0",
		JWTSecret: []byte(testJWTSecret),
		LogLevel:  0, // LevelInfo
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

// createSession is a test helper that creates a session and returns the session ID.
func createSession(t *testing.T, srv *Server, workspaceID, title string) string {
	t.Helper()
	rr := doRequest(t, srv, http.MethodPost, "/v1/sessions",
		map[string]string{"workspace_id": workspaceID, "title": title}, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create session: want 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode create session response: %v", err)
	}
	return resp.Session.ID
}

// attachBridge is a test helper that posts to /bridge and returns the BridgeResponse.
func attachBridge(t *testing.T, srv *Server, sessionID string) BridgeResponse {
	t.Helper()
	rr := doRequest(t, srv, http.MethodPost, "/v1/sessions/"+sessionID+"/bridge", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("attach bridge: want 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp BridgeResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode bridge response: %v", err)
	}
	return resp
}

// TestCreateSessionAndBridge creates a session, attaches a bridge, and verifies
// that a second bridge call increments the epoch.
func TestCreateSessionAndBridge(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Create session
	sessionID := createSession(t, srv, "ws_test", "Test")

	// 2. Assert session ID starts with "cse_"
	if !strings.HasPrefix(sessionID, "cse_") {
		t.Errorf("session ID: want prefix cse_, got %q", sessionID)
	}

	// 3. Attach bridge (first time)
	bridge1 := attachBridge(t, srv, sessionID)

	// 4. Assert worker_jwt is non-empty and worker_epoch == 1
	if bridge1.WorkerJWT == "" {
		t.Error("worker_jwt is empty")
	}
	if bridge1.WorkerEpoch != 1 {
		t.Errorf("worker_epoch: want 1, got %d", bridge1.WorkerEpoch)
	}

	// 5. Attach bridge again → worker_epoch should be 2
	bridge2 := attachBridge(t, srv, sessionID)
	if bridge2.WorkerEpoch != 2 {
		t.Errorf("worker_epoch (second bridge): want 2, got %d", bridge2.WorkerEpoch)
	}
}

// TestEventBatchAndReplay creates a session, attaches a bridge, posts events,
// and verifies that the events are persisted in the database.
func TestEventBatchAndReplay(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Create session + attach bridge
	sessionID := createSession(t, srv, "ws_test", "Event Test")
	bridge := attachBridge(t, srv, sessionID)

	// 2. POST events with JWT auth. Payload uses CC's SDKUserMessage shape
	// plus a `uuid` field — cc-broker's handleWorkerEvents extracts the
	// dedup key from payload.uuid. (Real CC events don't carry uuid; that's
	// a known limitation of the current bridge dedup model.)
	eventsReq := map[string]any{
		"worker_epoch": 1,
		"events": []map[string]any{
			{
				"payload": map[string]any{
					"uuid": "evt1",
					"type": "user",
					"message": map[string]any{
						"role":    "user",
						"content": "hello",
					},
					"parent_tool_use_id": nil,
					"session_id":         sessionID,
				},
				"ephemeral": false,
			},
		},
	}
	rr := doRequest(t, srv, http.MethodPost, "/v1/sessions/"+sessionID+"/worker/events",
		eventsReq, bridge.WorkerJWT)

	// 3. Assert 200
	if rr.Code != http.StatusOK {
		t.Fatalf("post events: want 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	// 4. Verify DB persistence via store.GetEventsSince
	events, err := srv.store.GetEventsSince(context.Background(), sessionID, 0, 10)
	if err != nil {
		t.Fatalf("GetEventsSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("GetEventsSince: want 1 event, got %d", len(events))
	}
	if events[0].EventID != "evt1" {
		t.Errorf("event_id: want evt1, got %q", events[0].EventID)
	}
}

// TestEpochMismatch creates a session, attaches a bridge, then posts events
// with a wrong epoch and verifies a 409 response.
func TestEpochMismatch(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Create session + bridge (epoch=1)
	sessionID := createSession(t, srv, "ws_test", "Epoch Test")
	bridge := attachBridge(t, srv, sessionID)

	// 2. POST events with wrong epoch
	eventsReq := map[string]any{
		"worker_epoch": 99,
		"events": []map[string]any{
			{
				"payload": map[string]any{
					"uuid": "evt_wrong",
					"type": "user",
					"message": map[string]any{
						"role":    "user",
						"content": "oops",
					},
					"parent_tool_use_id": nil,
					"session_id":         sessionID,
				},
				"ephemeral": false,
			},
		},
	}
	rr := doRequest(t, srv, http.MethodPost, "/v1/sessions/"+sessionID+"/worker/events",
		eventsReq, bridge.WorkerJWT)

	// 3. Assert 409
	if rr.Code != http.StatusConflict {
		t.Errorf("epoch mismatch: want 409, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestHeartbeat creates a session, attaches a bridge, and sends a heartbeat.
func TestHeartbeat(t *testing.T) {
	srv := setupTestServer(t)

	// 1. Create session + bridge
	sessionID := createSession(t, srv, "ws_test", "Heartbeat Test")
	bridge := attachBridge(t, srv, sessionID)

	// 2. POST heartbeat with JWT
	heartbeatReq := HeartbeatRequest{WorkerEpoch: 1}
	rr := doRequest(t, srv, http.MethodPost, "/v1/sessions/"+sessionID+"/worker/heartbeat",
		heartbeatReq, bridge.WorkerJWT)

	// 3. Assert 200
	if rr.Code != http.StatusOK {
		t.Errorf("heartbeat: want 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}
