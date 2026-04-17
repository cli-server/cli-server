// Package executortools implements the tool set served by the local
// executor over the executor-registry tunnel. Each tool is a pure Go
// function receiving JSON arguments and returning an ExecuteResponse.
package executortools

import (
	"context"
	"encoding/json"
	"time"
)

// ExecuteRequest mirrors the wire format that executor-registry forwards
// over the yamux stream (body of POST /tool/execute).
type ExecuteRequest struct {
	ExecutorID string          `json:"executor_id"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	TimeoutMs  int             `json:"timeout_ms,omitempty"`
}

// ExecuteResponse is the single unified response shape returned for every
// tool call. Output is free-form text; ExitCode is 0 on success.
type ExecuteResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// ToolExecutor dispatches tool calls to per-tool handlers rooted at WorkDir.
type ToolExecutor struct {
	WorkDir string
}

// New returns a new ToolExecutor rooted at workDir.
func New(workDir string) *ToolExecutor {
	return &ToolExecutor{WorkDir: workDir}
}

// Execute dispatches to the per-tool handler. Unknown tools return exit 1.
func (e *ToolExecutor) Execute(ctx context.Context, req ExecuteRequest) ExecuteResponse {
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}

	switch req.Tool {
	case "Bash":
		return e.bash(ctx, req.Arguments)
	case "Read":
		return e.read(ctx, req.Arguments)
	case "Write":
		return e.write(ctx, req.Arguments)
	case "LS":
		return e.ls(ctx, req.Arguments)
	case "Edit":
		return e.edit(ctx, req.Arguments)
	case "Glob":
		return e.glob(ctx, req.Arguments)
	case "Grep":
		return e.grep(ctx, req.Arguments)
	default:
		return errResponse("unknown tool: " + req.Tool)
	}
}

func errResponse(msg string) ExecuteResponse {
	return ExecuteResponse{Output: msg, ExitCode: 1}
}

func okResponse(output string) ExecuteResponse {
	return ExecuteResponse{Output: output, ExitCode: 0}
}
