package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentserver/agentserver/internal/envtools/bridge"
	"github.com/agentserver/agentserver/internal/envtools/nameresolver"
)

// ApplyPatchTool implements `apply_patch`. The patch text is parsed
// locally (in env-mcp) into structured FileOps; each op is then
// translated into fs/readFile + fs/writeFile + fs/remove + fs/copy
// RPCs on the remote.
//
// Per-file outcomes are reported as `path: ok` / `path: error: ...`
// lines so the LLM sees which files succeeded even on partial failure.
type ApplyPatchTool struct {
	pool     *bridge.Pool
	resolver *nameresolver.Resolver
}

func NewApplyPatchTool(pool *bridge.Pool, resolver *nameresolver.Resolver) *ApplyPatchTool {
	return &ApplyPatchTool{pool: pool, resolver: resolver}
}

var applyPatchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "environment_id": {"type": "string", "description": "Target environment's name from list_environments output"},
    "patch": {"type": "string", "description": "A codex apply_patch document beginning with '*** Begin Patch' and ending with '*** End Patch'"}
  },
  "required": ["environment_id", "patch"]
}`)

func (t *ApplyPatchTool) Name() string                 { return "apply_patch" }
func (t *ApplyPatchTool) InputSchema() json.RawMessage { return applyPatchSchema }
func (t *ApplyPatchTool) Description() string {
	return "Apply a codex-grammar patch to one or more files on the named environment. " +
		"Supports Add File, Update File (with @@ hunks), Delete File, and Move File."
}

func (t *ApplyPatchTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvironmentID string `json:"environment_id"`
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if a.EnvironmentID == "" || a.Patch == "" {
		return errResult("environment_id and patch are required"), nil
	}

	ops, err := ParsePatch(a.Patch)
	if err != nil {
		return errResult(err.Error()), nil
	}

	exeID, err := t.resolver.Resolve(ctx, a.EnvironmentID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	bc, err := t.pool.Get(ctx, exeID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q unavailable: %v", a.EnvironmentID, err)), nil
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

func (t *ApplyPatchTool) applyOp(ctx context.Context, bc *bridge.BridgeClient, op FileOp) error {
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
		params, _ := json.Marshal(bridge.FsRemoveParams{Path: op.Path})
		_, err := bc.Call(ctx, bridge.ExecMethodFsRemove, params)
		return err
	case OpMove:
		copyParams, _ := json.Marshal(bridge.FsCopyParams{SourcePath: op.Path, DestinationPath: op.NewPath})
		if _, err := bc.Call(ctx, bridge.ExecMethodFsCopy, copyParams); err != nil {
			return fmt.Errorf("fs/copy: %w", err)
		}
		rmParams, _ := json.Marshal(bridge.FsRemoveParams{Path: op.Path})
		_, err := bc.Call(ctx, bridge.ExecMethodFsRemove, rmParams)
		return err
	default:
		return fmt.Errorf("unknown op kind %d", op.Kind)
	}
}

// readFile is a thin helper around the fs/readFile RPC returning the
// decoded bytes.
func readFile(ctx context.Context, bc *bridge.BridgeClient, path string) ([]byte, error) {
	params, _ := json.Marshal(bridge.FsReadFileParams{Path: path})
	rawResp, err := bc.Call(ctx, bridge.ExecMethodFsReadFile, params)
	if err != nil {
		return nil, err
	}
	var r bridge.FsReadFileResult
	if err := json.Unmarshal(rawResp, &r); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(r.DataBase64)
}

// writeFile is a thin helper around fs/writeFile that base64-wraps
// and asks the server to create missing parent directories.
func writeFile(ctx context.Context, bc *bridge.BridgeClient, path string, data []byte) error {
	params, _ := json.Marshal(bridge.FsWriteFileParams{
		Path:          path,
		DataBase64:    base64.StdEncoding.EncodeToString(data),
		CreateMissing: true,
	})
	_, err := bc.Call(ctx, bridge.ExecMethodFsWriteFile, params)
	return err
}
