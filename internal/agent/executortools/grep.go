package executortools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func (e *ToolExecutor) grep(ctx context.Context, rawArgs json.RawMessage) ExecuteResponse {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path,omitempty"`
		Glob    string `json:"glob,omitempty"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponse("invalid arguments: " + err.Error())
	}
	if args.Pattern == "" {
		return errResponse("pattern is required")
	}

	searchPath := e.WorkDir
	if args.Path != "" {
		searchPath = resolvePath(e.WorkDir, args.Path)
	}

	if _, err := exec.LookPath("rg"); err == nil {
		return rgGrep(ctx, args.Pattern, args.Glob, searchPath)
	}
	return goGrep(args.Pattern, args.Glob, searchPath)
}

func rgGrep(ctx context.Context, pattern, glob, searchPath string) ExecuteResponse {
	cmdArgs := []string{"-n", "--no-heading"}
	if glob != "" {
		cmdArgs = append(cmdArgs, "-g", glob)
	}
	cmdArgs = append(cmdArgs, pattern, searchPath)

	cmd := exec.CommandContext(ctx, "rg", cmdArgs...)
	out, err := cmd.CombinedOutput()

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return errResponse("grep failed: " + err.Error())
		}
	}
	// rg exit 1 means "no matches" — not an error for our callers.
	if exitCode == 1 {
		exitCode = 0
	}
	return ExecuteResponse{Output: string(out), ExitCode: exitCode}
}

func goGrep(pattern, glob, searchPath string) ExecuteResponse {
	if _, err := os.Stat(searchPath); err != nil {
		return errResponse("grep: " + err.Error())
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return errResponse("invalid pattern: " + err.Error())
	}

	var globRE *regexp.Regexp
	if glob != "" {
		globRE, err = globToRegexp(glob)
		if err != nil {
			return errResponse("invalid glob: " + err.Error())
		}
	}

	var results []string
	_ = filepath.WalkDir(searchPath, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		if globRE != nil && !globRE.MatchString(filepath.Base(p)) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		// Skip likely-binary files — heuristic: a NUL byte in the first 8KB.
		head := data
		if len(head) > 8192 {
			head = head[:8192]
		}
		if bytes.IndexByte(head, 0) >= 0 {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				results = append(results, fmt.Sprintf("%s:%d:%s", p, i+1, line))
			}
		}
		return nil
	})
	return okResponse(strings.Join(results, "\n"))
}
