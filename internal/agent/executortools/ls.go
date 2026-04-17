package executortools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
)

func (e *ToolExecutor) ls(ctx context.Context, rawArgs json.RawMessage) ExecuteResponse {
	var args struct {
		Path string `json:"path,omitempty"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return errResponse("invalid arguments: " + err.Error())
		}
	}
	p := e.WorkDir
	if args.Path != "" {
		p = resolvePath(e.WorkDir, args.Path)
	}

	entries, err := os.ReadDir(p)
	if err != nil {
		return errResponse("ls failed: " + err.Error())
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}
	return okResponse(strings.Join(lines, "\n"))
}
