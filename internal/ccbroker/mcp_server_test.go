package ccbroker

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestMCPServer(t *testing.T) (*MCPServer, string) {
	t.Helper()
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewToolRouter(ToolRouterConfig{
		ExecutorRegistryURL: "http://localhost:9999", // won't be called in workspace tests
		AgentserverURL:      "http://localhost:9999",
		WorkspaceDir:        tmpDir,
		SessionID:           "test-session",
		WorkspaceID:         "test-workspace",
	}, logger)
	return NewMCPServer(router, logger), tmpDir
}

func mcpRequest(t *testing.T, srv http.Handler, method string, params interface{}) JSONRPCResponse {
	t.Helper()
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	rawParams := json.RawMessage(paramsJSON)
	rawID := json.RawMessage(`1`)
	body, err := json.Marshal(JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      &rawID,
		Method:  method,
		Params:  rawParams,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var resp JSONRPCResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestMCPInitialize(t *testing.T) {
	srv, _ := newTestMCPServer(t)
	resp := mcpRequest(t, srv, "initialize", map[string]interface{}{
		"protocolVersion": "2025-03-26",
		"clientInfo":      map[string]string{"name": "test"},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("result is not a map")
	}
	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("wrong protocol version: %v", result["protocolVersion"])
	}
	caps, ok := result["capabilities"].(map[string]interface{})
	if !ok {
		t.Error("capabilities is not a map")
	} else if _, hasTool := caps["tools"]; !hasTool {
		t.Error("capabilities missing tools key")
	}
}

func TestMCPToolsList(t *testing.T) {
	srv, _ := newTestMCPServer(t)
	resp := mcpRequest(t, srv, "tools/list", map[string]interface{}{})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("result is not a map")
	}
	tools, ok := result["tools"].([]interface{})
	if !ok {
		t.Fatal("tools is not a slice")
	}
	if len(tools) < 10 {
		t.Errorf("expected at least 10 tools, got %d", len(tools))
	}
	// Check specific tools exist.
	toolNames := make(map[string]bool)
	for _, tool := range tools {
		m, ok := tool.(map[string]interface{})
		if !ok {
			t.Fatal("tool entry is not a map")
		}
		name, _ := m["name"].(string)
		toolNames[name] = true
	}
	for _, expected := range []string{"remote_bash", "list_executors", "workspace_write", "send_message", "create_scheduled_task"} {
		if !toolNames[expected] {
			t.Errorf("missing tool: %s", expected)
		}
	}
}

func TestMCPWorkspaceTools(t *testing.T) {
	srv, tmpDir := newTestMCPServer(t)

	// Write a file.
	resp := mcpRequest(t, srv, "tools/call", MCPToolCallParams{
		Name: "workspace_write",
		Arguments: map[string]interface{}{
			"path":    "test.txt",
			"content": "hello world",
		},
	})
	if resp.Error != nil {
		t.Fatalf("workspace_write error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("workspace_write result is not a map")
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Errorf("workspace_write returned isError=true")
	}

	// Check the file exists on disk.
	data, err := os.ReadFile(filepath.Join(tmpDir, "test.txt"))
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("wrong content: %s", data)
	}

	// Read the file back via workspace_read.
	resp = mcpRequest(t, srv, "tools/call", MCPToolCallParams{
		Name:      "workspace_read",
		Arguments: map[string]interface{}{"path": "test.txt"},
	})
	if resp.Error != nil {
		t.Fatalf("workspace_read error: %+v", resp.Error)
	}
	result, ok = resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("workspace_read result is not a map")
	}
	content, _ := result["content"].([]interface{})
	if len(content) == 0 {
		t.Fatal("workspace_read returned no content blocks")
	}
	block, _ := content[0].(map[string]interface{})
	text, _ := block["text"].(string)
	if text != "hello world" {
		t.Errorf("workspace_read returned wrong text: %q", text)
	}

	// List files via workspace_ls.
	resp = mcpRequest(t, srv, "tools/call", MCPToolCallParams{
		Name:      "workspace_ls",
		Arguments: map[string]interface{}{"path": ""},
	})
	if resp.Error != nil {
		t.Fatalf("workspace_ls error: %+v", resp.Error)
	}
	result, ok = resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("workspace_ls result is not a map")
	}
	lsContent, _ := result["content"].([]interface{})
	if len(lsContent) == 0 {
		t.Fatal("workspace_ls returned no content blocks")
	}
	lsBlock, _ := lsContent[0].(map[string]interface{})
	lsText, _ := lsBlock["text"].(string)
	if !strings.Contains(lsText, "test.txt") {
		t.Errorf("workspace_ls output doesn't contain test.txt: %q", lsText)
	}
}

func TestMCPUnknownTool(t *testing.T) {
	srv, _ := newTestMCPServer(t)
	resp := mcpRequest(t, srv, "tools/call", MCPToolCallParams{
		Name:      "nonexistent_tool",
		Arguments: map[string]interface{}{},
	})
	// Router returns error for unknown tool, which the server maps to an RPC error.
	// Check for either RPC-level error or isError in result.
	if resp.Error != nil {
		// Acceptable: server returned an RPC-level error.
		return
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("result is neither error nor map")
	}
	if isErr, ok := result["isError"].(bool); !ok || !isErr {
		t.Error("expected isError=true for unknown tool")
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	srv, _ := newTestMCPServer(t)
	resp := mcpRequest(t, srv, "unknown/method", map[string]interface{}{})
	if resp.Error == nil {
		t.Error("expected RPC error for unknown method")
	}
	if resp.Error.Code != rpcMethodNotFound {
		t.Errorf("expected code %d, got %d", rpcMethodNotFound, resp.Error.Code)
	}
}

func TestMCPNotInitializedNotification(t *testing.T) {
	srv, _ := newTestMCPServer(t)
	// notifications/initialized should return 200 with no body error.
	rawID := json.RawMessage(`null`)
	body, _ := json.Marshal(JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      &rawID,
		Method:  "notifications/initialized",
	})
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}
