package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/agentserver/agentserver/internal/envtools/bridge"
	"github.com/agentserver/agentserver/internal/envtools/nameresolver"
)

// shellSchema is the JSON schema for the `shell` tool's arguments.
// environment_id and command (argv) are required; cwd is the executor's
// current directory when omitted (cross-platform safe — no /tmp default
// that would break on Windows executors).
var shellSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "environment_id": {"type": "string", "description": "Target environment's name from list_environments output (e.g. hpc-kunshan)"},
    "command": {"type": "array", "items": {"type": "string"}, "description": "argv as a list of strings; wrap shell commands with [\"sh\",\"-c\",...] or [\"cmd\",\"/c\",...] explicitly"},
    "cwd": {"type": "string", "description": "Working directory; executor's current dir when omitted"},
    "timeout_ms": {"type": "integer", "description": "Per-call wait cap; defaults to 60000"}
  },
  "required": ["environment_id", "command"]
}`)

// ShellTool implements the synchronous-shell MCP tool. Each call
// dispatches process/start on the selected executor then polls
// process/read until the process exits or the timeout elapses.
type ShellTool struct {
	pool     *bridge.Pool
	resolver *nameresolver.Resolver
	pidSeq   atomic.Uint64
}

func NewShellTool(pool *bridge.Pool, resolver *nameresolver.Resolver) *ShellTool {
	return &ShellTool{pool: pool, resolver: resolver}
}

func (t *ShellTool) Name() string { return "shell" }

func (t *ShellTool) Description() string {
	return "Run a shell command on the named environment and return its full output. " +
		"Use `list_environments` first to discover available environment names."
}

func (t *ShellTool) InputSchema() json.RawMessage { return shellSchema }

type shellArgs struct {
	EnvironmentID string   `json:"environment_id"`
	Command   []string `json:"command"`
	Cwd       string   `json:"cwd"`
	TimeoutMs int      `json:"timeout_ms"`
}

func (t *ShellTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a shellArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if a.EnvironmentID == "" {
		return errResult("environment_id is required; call list_environments to see available names"), nil
	}
	if len(a.Command) == 0 {
		return errResult("command must be a non-empty array"), nil
	}
	// Pass cwd through verbatim — empty string lets exec-server pick its
	// own current directory, which is the only sane cross-platform default
	// (the prior /tmp default broke Windows executors).
	exeID, err := t.resolver.Resolve(ctx, a.EnvironmentID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	bc, err := t.pool.Get(ctx, exeID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q (exe=%s) unavailable: %v", a.EnvironmentID, exeID, err)), nil
	}

	maxCycles := defaultMaxReadCycles
	if a.TimeoutMs > 0 {
		maxCycles = a.TimeoutMs / defaultReadWaitMs
		if maxCycles < 1 {
			maxCycles = 1
		}
	}

	pid := fmt.Sprintf("shell-%d", t.pidSeq.Add(1))
	// Pass cwd + env through verbatim. Empty env means exec-server inherits
	// its own environment, which is correct cross-platform behavior — the
	// prior hardcoded POSIX PATH broke Windows executors.
	startParams, _ := json.Marshal(bridge.ProcessStartParams{
		ProcessID: pid,
		Argv:      a.Command,
		Cwd:       a.Cwd,
		Env:       nil,
		TTY:       false,
		PipeStdin: false,
	})
	if _, err := bc.Call(ctx, bridge.ExecMethodProcessStart, startParams); err != nil {
		return errResult(fmt.Sprintf("[exec failed to start: %v]", err)), nil
	}

	var stdout, stderr strings.Builder
	var afterSeq uint64
	var exitCode *int
	var failure *string

	for cycle := 0; cycle < maxCycles; cycle++ {
		readParams, _ := json.Marshal(bridge.ProcessReadParams{
			ProcessID: pid, AfterSeq: afterSeq,
			MaxBytes: defaultMaxBytes, WaitMs: defaultReadWaitMs,
		})
		raw, err := bc.Call(ctx, bridge.ExecMethodProcessRead, readParams)
		if err != nil {
			return errResult(fmt.Sprintf("%s%s\n[exec read failed: %v]", stdout.String(), stderr.String(), err)), nil
		}
		var r bridge.ProcessReadResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return errResult(fmt.Sprintf("[exec read decode failed: %v]", err)), nil
		}
		for _, ch := range r.Chunks {
			data, err := base64.StdEncoding.DecodeString(ch.Chunk)
			if err != nil {
				continue
			}
			if ch.Stream == "stderr" {
				stderr.Write(data)
			} else {
				stdout.Write(data)
			}
		}
		afterSeq = r.NextSeq
		if r.Exited || r.Closed {
			exitCode = r.ExitCode
			failure = r.Failure
			break
		}
	}

	// Human-readable text fallback (for clients that don't read
	// structuredContent). Programmatic clients should use the
	// structuredContent fields below.
	var text strings.Builder
	if stdout.Len() > 0 {
		text.WriteString(stdout.String())
	}
	if stderr.Len() > 0 {
		if text.Len() > 0 {
			text.WriteString("\n--- stderr ---\n")
		}
		text.WriteString(stderr.String())
	}
	if failure != nil {
		fmt.Fprintf(&text, "\n[exec failure: %s]", *failure)
	}
	if exitCode != nil {
		fmt.Fprintf(&text, "\n[exit_code=%d]", *exitCode)
	} else {
		text.WriteString("\n[exec timed out without exit signal]")
	}

	// Build structuredContent — the SDK reads exit_code / stdout / stderr
	// directly from here, so non-zero exit must NOT be flagged as
	// IsError. IsError is reserved for "tool itself failed" — failure to
	// start the process, or the process never returned an exit signal
	// (timed out). A program exiting 1 because a search found no matches
	// is a legitimate result, not a tool error.
	sc := map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": nil,
	}
	if exitCode != nil {
		sc["exit_code"] = *exitCode
	}
	if failure != nil {
		sc["failure"] = *failure
	}
	scRaw, _ := json.Marshal(sc)

	isErr := failure != nil || exitCode == nil
	return MCPCallToolResult{
		Content:           []MCPToolContent{{Type: "text", Text: text.String()}},
		StructuredContent: scRaw,
		IsError:           isErr,
	}, nil
}

// errResult wraps a one-line error message as an MCP isError content.
func errResult(msg string) MCPCallToolResult {
	return MCPCallToolResult{
		Content: []MCPToolContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}

// Read-loop tuning constants (lifted from the v1 RunShell loop).
const (
	defaultMaxReadCycles = 240 // ~60s @ 250ms wait
	defaultReadWaitMs    = 250
	defaultMaxBytes      = 65536
)
