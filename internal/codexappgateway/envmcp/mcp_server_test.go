package envmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type stubTranslator struct {
	gotArgv []string
	gotCwd  string
	out     ShellResult
	err     error
}

func (s *stubTranslator) RunShell(_ context.Context, argv []string, cwd string) (ShellResult, error) {
	s.gotArgv = argv
	s.gotCwd = cwd
	return s.out, s.err
}

func driveServer(t *testing.T, srv *MCPServer, lines ...string) []map[string]any {
	t.Helper()
	in := bytes.NewBufferString(strings.Join(lines, "\n") + "\n")
	out := &bytes.Buffer{}
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var got []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("bad line %q: %v", ln, err)
		}
		got = append(got, m)
	}
	return got
}

func TestMCPServer_InitializeAndToolsList(t *testing.T) {
	srv := NewMCPServer("Daisy's MacBook", &stubTranslator{}, nil)
	got := driveServer(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(got) != 2 {
		t.Fatalf("got %d responses: %v", len(got), got)
	}
	res0 := got[0]["result"].(map[string]any)
	if res0["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v", res0["protocolVersion"])
	}
	res1 := got[1]["result"].(map[string]any)
	tools := res1["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "shell" {
		t.Errorf("tool name = %v", tool["name"])
	}
	if !strings.Contains(tool["description"].(string), "Daisy's MacBook") {
		t.Errorf("tool description missing executor: %v", tool["description"])
	}
}

func TestMCPServer_ToolsCallShell_DispatchesToTranslator(t *testing.T) {
	tr := &stubTranslator{out: ShellResult{Text: "ok\n[exit_code=0]", IsError: false}}
	srv := NewMCPServer("desc", tr, nil)
	got := driveServer(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shell","arguments":{"command":["ls","-la"],"cwd":"/srv"}}}`,
	)
	if len(got) != 2 {
		t.Fatalf("got %d responses", len(got))
	}
	if got := tr.gotArgv; len(got) != 2 || got[0] != "ls" || got[1] != "-la" {
		t.Errorf("argv = %v", tr.gotArgv)
	}
	if tr.gotCwd != "/srv" {
		t.Errorf("cwd = %q", tr.gotCwd)
	}
	res := got[1]["result"].(map[string]any)
	if res["isError"] != false {
		t.Errorf("isError = %v", res["isError"])
	}
	content := res["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || !strings.Contains(content["text"].(string), "ok") {
		t.Errorf("content = %v", content)
	}
}

func TestMCPServer_ToolsCall_UnknownTool_Error(t *testing.T) {
	srv := NewMCPServer("desc", &stubTranslator{}, nil)
	got := driveServer(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bogus","arguments":{}}}`,
	)
	if got[0]["error"] == nil {
		t.Fatalf("expected error response: %v", got[0])
	}
}

func TestMCPServer_PromptsAndResources_EmptyLists(t *testing.T) {
	srv := NewMCPServer("desc", &stubTranslator{}, nil)
	got := driveServer(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/templates/list"}`,
	)
	if len(got) != 3 {
		t.Fatalf("got %d responses", len(got))
	}
	for i, key := range []string{"prompts", "resources", "resourceTemplates"} {
		res := got[i]["result"].(map[string]any)
		arr, ok := res[key].([]any)
		if !ok || len(arr) != 0 {
			t.Errorf("response %d missing empty %s: %v", i, key, res)
		}
	}
}

func TestMCPServer_NotificationProducesNoReply(t *testing.T) {
	srv := NewMCPServer("desc", &stubTranslator{}, nil)
	got := driveServer(t, srv,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	)
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1", len(got))
	}
}
