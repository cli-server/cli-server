package executortools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func (e *ToolExecutor) edit(ctx context.Context, rawArgs json.RawMessage) ExecuteResponse {
	var args struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all,omitempty"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponse("invalid arguments: " + err.Error())
	}
	if args.FilePath == "" || args.OldString == "" {
		return errResponse("file_path and old_string are required")
	}

	p := resolvePath(e.WorkDir, args.FilePath)
	data, err := os.ReadFile(p)
	if err != nil {
		return errResponse("read failed: " + err.Error())
	}
	content := string(data)

	count := strings.Count(content, args.OldString)
	if count == 0 {
		return errResponse("old_string not found in file")
	}
	if count > 1 && !args.ReplaceAll {
		return errResponse(fmt.Sprintf("old_string appears %d times; set replace_all=true or provide more context", count))
	}

	var newContent string
	if args.ReplaceAll {
		newContent = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		newContent = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	if err := os.WriteFile(p, []byte(newContent), 0644); err != nil {
		return errResponse("write failed: " + err.Error())
	}
	return okResponse(fmt.Sprintf("edited %s (%d replacement(s))", args.FilePath, count))
}
