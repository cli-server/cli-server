package ccbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/runner"
	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// TestHandleProcessTurn_OrchestratesPipeline asserts the high-level flow:
// validation → session ensure → user-event insert → SDK pump → SSE done.
//
// The runner is faked to emit a scripted SDK message sequence; workspace
// Setup/Teardown are stubbed so no OpenViking calls happen and no real
// temp dir is created.
//
// NOTE: This test is currently t.Skip()ped because Server requires a real
// *Store backed by Postgres; no in-memory variant exists yet. The seam
// injection code is in place, scripted fake runner emits a 2-message
// stream, and the assertions are written out so the test can be activated
// when an in-memory store helper lands.
func TestHandleProcessTurn_OrchestratesPipeline(t *testing.T) {
	// Save and restore the package seams.
	origSetup := workspaceSetup
	origTeardown := workspaceTeardown
	origRun := runnerRun
	defer func() {
		workspaceSetup = origSetup
		workspaceTeardown = origTeardown
		runnerRun = origRun
	}()

	teardownCalled := atomic.Int32{}
	workspaceSetup = func(_ context.Context, wid, sid string, _ *workspace.S3Store) (*workspace.Workspace, error) {
		return &workspace.Workspace{
			WorkspaceID: wid,
			SessionID:   sid,
			TempDir:     "/tmp/fake",
			ClaudeDir:   "/tmp/fake/claude-config",
			ProjectDir:  "/tmp/fake/project",
			MemoryDir:   "/tmp/fake/claude-config/projects/ws_" + wid + "/memory",
		}, nil
	}
	workspaceTeardown = func(_ context.Context, _ *workspace.Workspace, _ *workspace.S3Store) error {
		teardownCalled.Add(1)
		return nil
	}

	// Scripted SDK output: one assistant message, one result.
	runnerRun = func(_ context.Context, _ *workspace.Workspace, _, _ string, _ runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
		ch := make(chan agentsdk.SDKMessage, 2)
		ch <- agentsdk.SDKMessage{
			Type: "assistant",
			Raw:  json.RawMessage(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`),
		}
		ch <- agentsdk.SDKMessage{
			Type:    "result",
			Subtype: "success",
			Raw:     json.RawMessage(`{"type":"result","subtype":"success","is_error":false}`),
		}
		close(ch)
		return ch, nil
	}

	srv := buildTestServer(t)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"session_id":   "cse_test1",
		"workspace_id": "ws_test",
		"user_message": "hello",
	})
	resp, err := http.Post(srv.URL+"/api/turns", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/turns: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"event_type":"done"`) {
		t.Fatalf("response missing done sentinel: %q", out)
	}
	if got := teardownCalled.Load(); got != 1 {
		t.Errorf("Teardown called %d times, want 1", got)
	}
}

// buildTestServer wires a Server backed by an in-memory store + SSE broker
// suitable for handler tests. Returns the httptest.Server.
//
// Currently t.Skip()ped: Store requires a Postgres *sql.DB. When an
// in-memory store helper is added (e.g. a fakeStore implementing the same
// query methods), replace the t.Skip() with a wired *Server and activate
// the assertions in TestHandleProcessTurn_OrchestratesPipeline.
func buildTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Skip("buildTestServer not implemented: Store requires a real Postgres connection; " +
		"orchestration test is held here until an in-memory store helper is available")
	return nil
}
