package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/agentserver/agentserver/internal/envtools/bridge"
	"github.com/agentserver/agentserver/internal/envtools/nameresolver"
)

// ReadFileTool implements `read_file` via exec-server fs/readFile.
// Offset/limit are applied to the decoded bytes after fetching the
// full file from the remote, matching codex's local read_file
// semantics (exec-server doesn't support partial reads server-side).
type ReadFileTool struct {
	pool     *bridge.Pool
	resolver *nameresolver.Resolver
}

func NewReadFileTool(pool *bridge.Pool, resolver *nameresolver.Resolver) *ReadFileTool {
	return &ReadFileTool{pool: pool, resolver: resolver}
}

var readFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "environment_id": {"type": "string", "description": "Target environment's name from list_environments output"},
    "path": {"type": "string", "description": "Absolute path on the executor"},
    "offset": {"type": "integer", "description": "Byte offset to start reading from"},
    "limit": {"type": "integer", "description": "Max bytes to return; 0 = whole file"}
  },
  "required": ["environment_id", "path"]
}`)

func (t *ReadFileTool) Name() string                 { return "read_file" }
func (t *ReadFileTool) InputSchema() json.RawMessage { return readFileSchema }
func (t *ReadFileTool) Description() string {
	return "Read a file from the named environment. Returns the file contents as text."
}

func (t *ReadFileTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvironmentID string `json:"environment_id"`
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if a.EnvironmentID == "" || a.Path == "" {
		return errResult("environment_id and path are required"), nil
	}
	exeID, err := t.resolver.Resolve(ctx, a.EnvironmentID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	bc, err := t.pool.Get(ctx, exeID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q unavailable: %v", a.EnvironmentID, err)), nil
	}
	params, _ := json.Marshal(bridge.FsReadFileParams{Path: a.Path})
	rawResp, err := bc.Call(ctx, bridge.ExecMethodFsReadFile, params)
	if err != nil {
		return errResult(fmt.Sprintf("read_file failed: %v", err)), nil
	}
	var r bridge.FsReadFileResult
	if err := json.Unmarshal(rawResp, &r); err != nil {
		return errResult(fmt.Sprintf("read_file decode: %v", err)), nil
	}
	data, err := base64.StdEncoding.DecodeString(r.DataBase64)
	if err != nil {
		return errResult(fmt.Sprintf("read_file base64: %v", err)), nil
	}
	if a.Offset < 0 {
		a.Offset = 0
	}
	if a.Offset > 0 {
		if a.Offset >= len(data) {
			data = nil
		} else {
			data = data[a.Offset:]
		}
	}
	if a.Limit > 0 && a.Limit < len(data) {
		data = data[:a.Limit]
	}
	// Return the file as base64 in both Content (so the SDK decoder finds
	// it in items[].text) and structuredContent.encoding (so the decoder
	// knows to decode). Returning `string(data)` for binary files corrupted
	// any byte sequence not valid UTF-8 — base64 keeps the round-trip clean.
	encoded := base64.StdEncoding.EncodeToString(data)
	sc, _ := json.Marshal(map[string]any{"encoding": "base64", "path": a.Path})
	return MCPCallToolResult{
		Content:           []MCPToolContent{{Type: "text", Text: encoded}},
		StructuredContent: sc,
	}, nil
}

// WriteFileTool implements `write_file` via exec-server fs/writeFile.
// The caller passes content_b64 (base64-encoded raw bytes); we validate
// the encoding locally and forward the same string to the bridge in
// bridge.FsWriteFileParams.DataBase64 — exec-server expects base64 on
// the wire, so a re-encode would be wasted work.
type WriteFileTool struct {
	pool     *bridge.Pool
	resolver *nameresolver.Resolver
}

func NewWriteFileTool(pool *bridge.Pool, resolver *nameresolver.Resolver) *WriteFileTool {
	return &WriteFileTool{pool: pool, resolver: resolver}
}

var writeFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "environment_id": {"type": "string", "description": "Target environment's name from list_environments output"},
    "path":   {"type": "string", "description": "Absolute path on the executor"},
    "content_b64": {"type": "string", "description": "Base64-encoded file content"}
  },
  "required": ["environment_id", "path", "content_b64"]
}`)

func (t *WriteFileTool) Name() string                 { return "write_file" }
func (t *WriteFileTool) InputSchema() json.RawMessage { return writeFileSchema }
func (t *WriteFileTool) Description() string {
	return "Write a file to the named environment. Content must be base64-encoded raw bytes."
}

func (t *WriteFileTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvironmentID string `json:"environment_id"`
		Path          string `json:"path"`
		ContentB64    string `json:"content_b64"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if a.EnvironmentID == "" || a.Path == "" {
		return errResult("environment_id and path are required"), nil
	}
	// Validate locally so a malformed body fails fast with a clear
	// error rather than bouncing off the exec-server's generic decoder.
	if _, err := base64.StdEncoding.DecodeString(a.ContentB64); err != nil {
		return errResult("content_b64 invalid: " + err.Error()), nil
	}
	exeID, err := t.resolver.Resolve(ctx, a.EnvironmentID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	bc, err := t.pool.Get(ctx, exeID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q unavailable: %v", a.EnvironmentID, err)), nil
	}
	params, _ := json.Marshal(bridge.FsWriteFileParams{
		Path:       a.Path,
		DataBase64: a.ContentB64,
	})
	if _, err := bc.Call(ctx, bridge.ExecMethodFsWriteFile, params); err != nil {
		return errResult(fmt.Sprintf("write_file failed: %v", err)), nil
	}
	return MCPCallToolResult{
		Content: []MCPToolContent{{Type: "text", Text: "ok"}},
	}, nil
}
