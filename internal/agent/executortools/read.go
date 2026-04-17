package executortools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
)

func (e *ToolExecutor) read(ctx context.Context, rawArgs json.RawMessage) ExecuteResponse {
	var args struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset,omitempty"`
		Limit    int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponse("invalid arguments: " + err.Error())
	}
	if args.FilePath == "" {
		return errResponse("file_path is required")
	}

	data, err := os.ReadFile(resolvePath(e.WorkDir, args.FilePath))
	if err != nil {
		return errResponse("read failed: " + err.Error())
	}

	if args.Offset <= 0 && args.Limit <= 0 {
		return okResponse(string(data))
	}

	lines := strings.Split(string(data), "\n")
	start := args.Offset
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if args.Limit > 0 && start+args.Limit < end {
		end = start + args.Limit
	}
	return okResponse(strings.Join(lines[start:end], "\n"))
}
