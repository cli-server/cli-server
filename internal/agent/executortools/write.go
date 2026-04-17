package executortools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func (e *ToolExecutor) write(ctx context.Context, rawArgs json.RawMessage) ExecuteResponse {
	var args struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponse("invalid arguments: " + err.Error())
	}
	if args.FilePath == "" {
		return errResponse("file_path is required")
	}

	p := resolvePath(e.WorkDir, args.FilePath)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return errResponse("mkdir failed: " + err.Error())
	}
	if err := os.WriteFile(p, []byte(args.Content), 0644); err != nil {
		return errResponse("write failed: " + err.Error())
	}
	return okResponse(fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.FilePath))
}
