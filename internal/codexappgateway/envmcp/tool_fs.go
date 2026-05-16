package envmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// ReadFileTool implements `read_file` via exec-server fs/readFile.
// Offset/limit are applied to the decoded bytes after fetching the
// full file from the remote, matching codex's local read_file
// semantics (exec-server doesn't support partial reads server-side).
type ReadFileTool struct {
	pool *BridgePool
}

func NewReadFileTool(pool *BridgePool) *ReadFileTool {
	return &ReadFileTool{pool: pool}
}

var readFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "env_id": {"type": "string"},
    "path": {"type": "string", "description": "Absolute path on the executor"},
    "offset": {"type": "integer", "description": "Byte offset to start reading from"},
    "limit": {"type": "integer", "description": "Max bytes to return; 0 = whole file"}
  },
  "required": ["env_id", "path"]
}`)

func (t *ReadFileTool) Name() string                 { return "read_file" }
func (t *ReadFileTool) InputSchema() json.RawMessage { return readFileSchema }
func (t *ReadFileTool) Description() string {
	return "Read a file from the named environment. Returns the file contents as text."
}

func (t *ReadFileTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvID  string `json:"env_id"`
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if a.EnvID == "" || a.Path == "" {
		return errResult("env_id and path are required"), nil
	}
	bc, err := t.pool.Get(ctx, a.EnvID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q unavailable: %v", a.EnvID, err)), nil
	}
	params, _ := json.Marshal(FsReadFileParams{Path: a.Path})
	rawResp, err := bc.Call(ctx, ExecMethodFsReadFile, params)
	if err != nil {
		return errResult(fmt.Sprintf("read_file failed: %v", err)), nil
	}
	var r FsReadFileResult
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
	return MCPCallToolResult{
		Content: []MCPToolContent{{Type: "text", Text: string(data)}},
	}, nil
}
