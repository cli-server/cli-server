package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
)

// toolByName finds the first McpTool with the given name in the slice.
func toolByName(tools []agentsdk.McpTool, name string) *agentsdk.McpTool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func TestForwardExecute_RemoteBash(t *testing.T) {
	type executeRequest struct {
		ExecutorID string          `json:"executor_id"`
		Tool       string          `json:"tool"`
		Arguments  json.RawMessage `json:"arguments"`
	}

	var captured executeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// (a) Assert URL was hit at the right path with POST.
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/execute" {
			t.Errorf("path: got %q, want /api/execute", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request body: %v", err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":"hello","exit_code":0}`))
	}))
	defer srv.Close()

	tctx := &Context{
		WorkspaceID:         "ws-123",
		ExecutorRegistryURL: srv.URL,
		HTTP:                srv.Client(),
	}

	tools := executorTools(tctx)
	remoteBash := toolByName(tools, "remote_bash")
	if remoteBash == nil {
		t.Fatal("remote_bash tool not registered")
	}

	inputJSON := json.RawMessage(`{
		"executor_id": "exe_abc",
		"command":     "echo hello",
		"description": "test echo"
	}`)

	result, err := remoteBash.Handler(context.Background(), inputJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content[0].Text)
	}

	// (a) Request was forwarded — captured fields are populated.
	if captured.ExecutorID == "" {
		t.Fatal("executor_id not in captured body — HTTP request was not sent")
	}

	// (b) Body shape: executor_id at top level.
	if captured.ExecutorID != "exe_abc" {
		t.Errorf("executor_id: got %q, want exe_abc", captured.ExecutorID)
	}

	// (d) tool field is capitalised CC tool name "Bash".
	if captured.Tool != "Bash" {
		t.Errorf("tool: got %q, want Bash", captured.Tool)
	}

	// (c) executor_id must be absent from forwarded arguments.
	var argMap map[string]json.RawMessage
	if err := json.Unmarshal(captured.Arguments, &argMap); err != nil {
		t.Fatalf("unmarshal captured arguments: %v", err)
	}
	if _, present := argMap["executor_id"]; present {
		t.Error("executor_id must be stripped from arguments, but was present")
	}
	if _, present := argMap["command"]; !present {
		t.Error("command must be present in forwarded arguments")
	}

	// Response body is forwarded verbatim as text content.
	if len(result.Content) == 0 {
		t.Fatal("result.Content is empty")
	}
	if result.Content[0].Text == "" {
		t.Error("result text content must not be empty")
	}
}

func TestListExecutors_URLAndWorkspaceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: got %q, want GET", r.Method)
		}
		if r.URL.Path != "/api/executors" {
			t.Errorf("path: got %q, want /api/executors", r.URL.Path)
		}
		wid := r.URL.Query().Get("workspace_id")
		if wid != "ws-456" {
			t.Errorf("workspace_id query param: got %q, want ws-456", wid)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"exe_1","status":"online"}]`))
	}))
	defer srv.Close()

	tctx := &Context{
		WorkspaceID:         "ws-456",
		ExecutorRegistryURL: srv.URL,
		HTTP:                srv.Client(),
	}

	tools := executorTools(tctx)
	listTool := toolByName(tools, "list_executors")
	if listTool == nil {
		t.Fatal("list_executors tool not registered")
	}

	result, err := listTool.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content[0].Text)
	}
	if len(result.Content) == 0 {
		t.Fatal("result.Content is empty")
	}
	const want = `[{"id":"exe_1","status":"online"}]`
	if result.Content[0].Text != want {
		t.Errorf("body: got %q, want %q", result.Content[0].Text, want)
	}
}

func TestForwardExecute_MissingExecutorID(t *testing.T) {
	// No HTTP server needed — error is caught before any network call.
	tctx := &Context{
		WorkspaceID:         "ws-999",
		ExecutorRegistryURL: "http://unused",
		HTTP:                http.DefaultClient,
	}

	tools := executorTools(tctx)
	bashTool := toolByName(tools, "remote_bash")
	if bashTool == nil {
		t.Fatal("remote_bash tool not registered")
	}

	result, err := bashTool.Handler(context.Background(), json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatalf("unexpected non-tool error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true when executor_id is missing or empty")
	}
}

func TestExecutorTools_RegistersAll8Tools(t *testing.T) {
	tctx := &Context{
		ExecutorRegistryURL: "http://unused",
		HTTP:                http.DefaultClient,
	}
	tools := executorTools(tctx)

	want := []string{
		"remote_bash", "remote_read", "remote_edit", "remote_write",
		"remote_glob", "remote_grep", "remote_ls", "list_executors",
	}
	if len(tools) != len(want) {
		t.Fatalf("tool count: got %d, want %d", len(tools), len(want))
	}
	for _, name := range want {
		if toolByName(tools, name) == nil {
			t.Errorf("missing tool: %s", name)
		}
	}
}

// textOfResult extracts the first content text from a McpToolResult.
func textOfResult(r *agentsdk.McpToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

// TestRemoteBash_BlockedByGateAsk verifies that gateCheck emits a
// permission_request event in "ask" mode and returns an IsError result
// with a "permission_denied" reason code when the user denies.
func TestRemoteBash_BlockedByGateAsk(t *testing.T) {
	notified := make(chan Event, 4)
	g := NewGate(func(_ string, e Event) {
		select {
		case notified <- e:
		default:
		}
	})
	tctx := &Context{
		SessionID:           "s",
		WorkspaceID:         "ws",
		Gate:                g,
		PermissionMode:      "ask",
		CreatorUserID:       "u",
		CurrentTurnID:       "t1",
		ExecutorRegistryURL: "http://exec-reg-must-not-be-called",
		HTTP:                &http.Client{Timeout: time.Second},
	}

	// Stub out the executor lookup so no real HTTP call is made.
	origLookup := lookupExecutor
	lookupExecutor = func(_ context.Context, _ *Context, _ string) (lookupResult, error) {
		return lookupResult{OwnerUserID: "u", SharedToWorkspace: false}, nil
	}
	defer func() { lookupExecutor = origLookup }()

	rawArgs, _ := json.Marshal(map[string]string{"command": "ls"})
	done := make(chan *agentsdk.McpToolResult, 1)
	go func() {
		res := gateCheck(context.Background(), tctx, "remote_bash", "exe_a", json.RawMessage(rawArgs))
		done <- res
	}()

	// Wait for the permission_request event, then deny it.
	select {
	case ev := <-notified:
		if ev.Type != "permission_request" {
			t.Fatalf("first event = %q, want permission_request", ev.Type)
		}
		if err := g.Resolve(ev.PermissionID, Decision{Verdict: "deny", Scope: "once"}); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no permission_request emitted within timeout")
	}

	select {
	case res := <-done:
		if res == nil || !res.IsError {
			t.Errorf("expected IsError result, got %+v", res)
		}
		if !strings.Contains(textOfResult(res), "permission_denied") {
			t.Errorf("error text missing reason code: %s", textOfResult(res))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateCheck didn't return after deny")
	}
}

// TestRemoteBash_BypassDispatchesToRegistry verifies that in "bypass" mode
// the gate is skipped and the call is forwarded to executor-registry.
func TestRemoteBash_BypassDispatchesToRegistry(t *testing.T) {
	var registryHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/execute") {
			registryHit = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"output":"hello","exit_code":0}`))
		}
	}))
	defer srv.Close()

	tctx := &Context{
		SessionID:           "s",
		Gate:                NewGate(func(_ string, _ Event) {}),
		PermissionMode:      "bypass",
		CreatorUserID:       "u",
		ExecutorRegistryURL: srv.URL,
		HTTP:                srv.Client(),
	}

	origLookup := lookupExecutor
	lookupExecutor = func(_ context.Context, _ *Context, _ string) (lookupResult, error) {
		return lookupResult{OwnerUserID: "u"}, nil
	}
	defer func() { lookupExecutor = origLookup }()

	res, err := forwardExecute(context.Background(), tctx, "remote_bash", "Bash",
		remoteBashInput{ExecutorID: "exe_a", Command: "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.IsError {
		t.Errorf("expected non-error result, got %+v", res)
	}
	if !registryHit {
		t.Error("expected dispatch to executor-registry, but /api/execute was never hit")
	}
}
