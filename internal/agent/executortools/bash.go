package executortools

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
)

func (e *ToolExecutor) bash(ctx context.Context, rawArgs json.RawMessage) ExecuteResponse {
	var args struct {
		Command     string `json:"command"`
		Timeout     int    `json:"timeout,omitempty"`
		Description string `json:"description,omitempty"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponse("invalid arguments: " + err.Error())
	}
	if args.Command == "" {
		return errResponse("command is required")
	}

	cmd := exec.CommandContext(ctx, "bash", "-lc", args.Command)
	cmd.Dir = e.WorkDir
	out, err := cmd.CombinedOutput()

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			out = append(out, []byte("\n"+err.Error())...)
		}
	}
	return ExecuteResponse{Output: string(out), ExitCode: exitCode}
}
