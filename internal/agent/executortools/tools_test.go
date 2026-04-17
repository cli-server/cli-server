package executortools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, e *ToolExecutor, tool, argsJSON string) ExecuteResponse {
	t.Helper()
	return e.Execute(context.Background(), ExecuteRequest{
		Tool:      tool,
		Arguments: json.RawMessage(argsJSON),
	})
}

func TestBash(t *testing.T) {
	dir := t.TempDir()
	e := New(dir)
	resp := run(t, e, "Bash", `{"command":"echo hello"}`)
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d; output=%s", resp.ExitCode, resp.Output)
	}
	if !strings.Contains(resp.Output, "hello") {
		t.Fatalf("expected 'hello' in output, got %q", resp.Output)
	}
}

func TestBashNonzeroExit(t *testing.T) {
	e := New(t.TempDir())
	resp := run(t, e, "Bash", `{"command":"exit 3"}`)
	if resp.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d", resp.ExitCode)
	}
}

func TestWriteReadLS(t *testing.T) {
	dir := t.TempDir()
	e := New(dir)

	if resp := run(t, e, "Write", `{"file_path":"test.txt","content":"hello world"}`); resp.ExitCode != 0 {
		t.Fatalf("write failed: %s", resp.Output)
	}

	resp := run(t, e, "Read", `{"file_path":"test.txt"}`)
	if resp.ExitCode != 0 {
		t.Fatalf("read failed: %s", resp.Output)
	}
	if resp.Output != "hello world" {
		t.Fatalf("read mismatch: got %q", resp.Output)
	}

	resp = run(t, e, "LS", `{}`)
	if resp.ExitCode != 0 {
		t.Fatalf("ls failed: %s", resp.Output)
	}
	if !strings.Contains(resp.Output, "test.txt") {
		t.Fatalf("ls missing test.txt: %s", resp.Output)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lines.txt"), []byte("a\nb\nc\nd\ne"), 0644); err != nil {
		t.Fatal(err)
	}
	e := New(dir)
	resp := run(t, e, "Read", `{"file_path":"lines.txt","offset":1,"limit":2}`)
	if resp.ExitCode != 0 {
		t.Fatalf("read failed: %s", resp.Output)
	}
	if resp.Output != "b\nc" {
		t.Fatalf("expected 'b\\nc', got %q", resp.Output)
	}
}

func TestEdit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("foo bar baz"), 0644); err != nil {
		t.Fatal(err)
	}
	e := New(dir)

	if resp := run(t, e, "Edit", `{"file_path":"f.txt","old_string":"bar","new_string":"qux"}`); resp.ExitCode != 0 {
		t.Fatalf("edit failed: %s", resp.Output)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "foo qux baz" {
		t.Fatalf("edit result: got %q", data)
	}
}

func TestEditMissingString(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("abc"), 0644)
	e := New(dir)
	resp := run(t, e, "Edit", `{"file_path":"f.txt","old_string":"xyz","new_string":"q"}`)
	if resp.ExitCode == 0 {
		t.Fatalf("expected failure when old_string missing, got success: %s", resp.Output)
	}
}

func TestEditNonUniqueRequiresReplaceAll(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x x x"), 0644)
	e := New(dir)

	resp := run(t, e, "Edit", `{"file_path":"f.txt","old_string":"x","new_string":"y"}`)
	if resp.ExitCode == 0 {
		t.Fatalf("expected failure on non-unique match without replace_all")
	}

	resp = run(t, e, "Edit", `{"file_path":"f.txt","old_string":"x","new_string":"y","replace_all":true}`)
	if resp.ExitCode != 0 {
		t.Fatalf("replace_all failed: %s", resp.Output)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "y y y" {
		t.Fatalf("replace_all result: got %q", data)
	}
}

func TestGlob(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("not go"), 0644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "d.go"), []byte("package sub"), 0644)
	e := New(dir)

	resp := run(t, e, "Glob", `{"pattern":"*.go"}`)
	if resp.ExitCode != 0 {
		t.Fatalf("glob failed: %s", resp.Output)
	}
	if !strings.Contains(resp.Output, "a.go") || !strings.Contains(resp.Output, "b.go") {
		t.Fatalf("glob missing top-level go files: %s", resp.Output)
	}
	if strings.Contains(resp.Output, "c.txt") {
		t.Fatalf("glob matched txt: %s", resp.Output)
	}

	resp = run(t, e, "Glob", `{"pattern":"**/*.go"}`)
	if resp.ExitCode != 0 {
		t.Fatalf("recursive glob failed: %s", resp.Output)
	}
	if !strings.Contains(resp.Output, "sub/d.go") {
		t.Fatalf("recursive glob missed sub/d.go: %s", resp.Output)
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\nfunc hello() {}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package main\nfunc world() {}\n"), 0644)
	e := New(dir)

	resp := run(t, e, "Grep", `{"pattern":"hello"}`)
	if resp.ExitCode != 0 {
		t.Fatalf("grep failed: %s", resp.Output)
	}
	if !strings.Contains(resp.Output, "hello") || !strings.Contains(resp.Output, "a.go") {
		t.Fatalf("grep missed a.go/hello: %s", resp.Output)
	}
}

func TestUnknownTool(t *testing.T) {
	e := New(t.TempDir())
	resp := run(t, e, "Banana", `{}`)
	if resp.ExitCode == 0 {
		t.Fatalf("expected error for unknown tool, got %s", resp.Output)
	}
	if !strings.Contains(resp.Output, "unknown tool") {
		t.Fatalf("unexpected error: %s", resp.Output)
	}
}
