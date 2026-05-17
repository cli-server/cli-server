package envmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ApplyPatchTool implements `apply_patch`. The patch text is parsed
// locally (in env-mcp) into structured FileOps; each op is then
// translated into fs/readFile + fs/writeFile + fs/remove + fs/copy
// RPCs on the remote.
//
// Per-file outcomes are reported as `path: ok` / `path: error: ...`
// lines so the LLM sees which files succeeded even on partial failure.
type ApplyPatchTool struct {
	pool *BridgePool
}

func NewApplyPatchTool(pool *BridgePool) *ApplyPatchTool {
	return &ApplyPatchTool{pool: pool}
}

var applyPatchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "env_id": {"type": "string", "description": "Target environment's exe_id (from list_environments output). NOT the description."},
    "patch": {"type": "string", "description": "A codex apply_patch document beginning with '*** Begin Patch' and ending with '*** End Patch'"}
  },
  "required": ["env_id", "patch"]
}`)

func (t *ApplyPatchTool) Name() string                 { return "apply_patch" }
func (t *ApplyPatchTool) InputSchema() json.RawMessage { return applyPatchSchema }
func (t *ApplyPatchTool) Description() string {
	return "Apply a codex-grammar patch to one or more files on the named environment. " +
		"Supports Add File, Update File (with @@ hunks), Delete File, and Move File."
}

func (t *ApplyPatchTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvID string `json:"env_id"`
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if a.EnvID == "" || a.Patch == "" {
		return errResult("env_id and patch are required"), nil
	}

	ops, err := ParsePatch(a.Patch)
	if err != nil {
		return errResult(err.Error()), nil
	}

	bc, err := t.pool.Get(ctx, a.EnvID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q unavailable: %v", a.EnvID, err)), nil
	}

	var report strings.Builder
	anyErr := false
	for _, op := range ops {
		if err := t.applyOp(ctx, bc, op); err != nil {
			fmt.Fprintf(&report, "%s: error: %v\n", op.Path, err)
			anyErr = true
		} else {
			fmt.Fprintf(&report, "%s: ok\n", op.Path)
		}
	}

	return MCPCallToolResult{
		Content: []MCPToolContent{{Type: "text", Text: strings.TrimRight(report.String(), "\n")}},
		IsError: anyErr,
	}, nil
}

func (t *ApplyPatchTool) applyOp(ctx context.Context, bc *BridgeClient, op FileOp) error {
	switch op.Kind {
	case OpAdd:
		return writeFile(ctx, bc, op.Path, []byte(op.Content))
	case OpUpdate:
		raw, err := readFile(ctx, bc, op.Path)
		if err != nil {
			return fmt.Errorf("readFile: %w", err)
		}
		patched, err := ApplyHunks(string(raw), op.Hunks)
		if err != nil {
			return err
		}
		return writeFile(ctx, bc, op.Path, []byte(patched))
	case OpDelete:
		params, _ := json.Marshal(FsRemoveParams{Path: op.Path})
		_, err := bc.Call(ctx, ExecMethodFsRemove, params)
		return err
	case OpMove:
		copyParams, _ := json.Marshal(FsCopyParams{SourcePath: op.Path, DestinationPath: op.NewPath})
		if _, err := bc.Call(ctx, ExecMethodFsCopy, copyParams); err != nil {
			return fmt.Errorf("fs/copy: %w", err)
		}
		rmParams, _ := json.Marshal(FsRemoveParams{Path: op.Path})
		_, err := bc.Call(ctx, ExecMethodFsRemove, rmParams)
		return err
	default:
		return fmt.Errorf("unknown op kind %d", op.Kind)
	}
}

// readFile is a thin helper around the fs/readFile RPC returning the
// decoded bytes.
func readFile(ctx context.Context, bc *BridgeClient, path string) ([]byte, error) {
	params, _ := json.Marshal(FsReadFileParams{Path: path})
	rawResp, err := bc.Call(ctx, ExecMethodFsReadFile, params)
	if err != nil {
		return nil, err
	}
	var r FsReadFileResult
	if err := json.Unmarshal(rawResp, &r); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(r.DataBase64)
}

// writeFile is a thin helper around fs/writeFile that base64-wraps
// and asks the server to create missing parent directories.
func writeFile(ctx context.Context, bc *BridgeClient, path string, data []byte) error {
	params, _ := json.Marshal(FsWriteFileParams{
		Path:          path,
		DataBase64:    base64.StdEncoding.EncodeToString(data),
		CreateMissing: true,
	})
	_, err := bc.Call(ctx, ExecMethodFsWriteFile, params)
	return err
}
