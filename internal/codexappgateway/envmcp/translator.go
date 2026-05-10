package envmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// BridgeCaller is the slice of BridgeClient that Translator needs.
// Defined as an interface so tests can script call sequences.
type BridgeCaller interface {
	Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
	Notify(ctx context.Context, method string, params json.RawMessage) error
}

// Translator turns MCP shell tool calls into exec-server JSON-RPC
// sequences (process/start, then process/read until exited or closed).
type Translator struct {
	bridge BridgeCaller
	pidSeq atomic.Uint64
}

// ShellResult is what RunShell returns; mapped into MCP CallToolResult
// by the caller (mcp_server.go).
type ShellResult struct {
	Text    string
	IsError bool
}

const (
	defaultMaxReadCycles = 240 // ~60s @ 250ms wait
	defaultReadWaitMs    = 250
	defaultMaxBytes      = 65536
)

func NewTranslator(b BridgeCaller) *Translator { return &Translator{bridge: b} }

// RunShell runs argv on the bound executor in cwd and returns the
// aggregated output. Never returns ctx-independent errors — transport
// failures surface as `IsError=true` with the failure in Text.
func (t *Translator) RunShell(ctx context.Context, argv []string, cwd string) (ShellResult, error) {
	if len(argv) == 0 {
		return ShellResult{}, errors.New("RunShell: empty argv")
	}
	pid := fmt.Sprintf("envmcp-%d", t.pidSeq.Add(1))

	startParams, err := json.Marshal(ProcessStartParams{
		ProcessID: pid,
		Argv:      argv,
		Cwd:       cwd,
		Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
		TTY:       false,
		PipeStdin: false,
		Arg0:      nil,
	})
	if err != nil {
		return ShellResult{}, fmt.Errorf("marshal process/start: %w", err)
	}
	if _, err := t.bridge.Call(ctx, ExecMethodProcessStart, startParams); err != nil {
		return ShellResult{
			Text:    fmt.Sprintf("[exec failed to start: %v]", err),
			IsError: true,
		}, nil
	}

	var stdout, stderr strings.Builder
	var afterSeq uint64
	var exitCode *int
	var failure *string

	for cycle := 0; cycle < defaultMaxReadCycles; cycle++ {
		readParams, _ := json.Marshal(ProcessReadParams{
			ProcessID: pid,
			AfterSeq:  afterSeq,
			MaxBytes:  defaultMaxBytes,
			WaitMs:    defaultReadWaitMs,
		})
		raw, err := t.bridge.Call(ctx, ExecMethodProcessRead, readParams)
		if err != nil {
			return ShellResult{
				Text:    fmt.Sprintf("%s%s\n[exec read failed: %v]", stdout.String(), stderr.String(), err),
				IsError: true,
			}, nil
		}
		var r ProcessReadResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return ShellResult{
				Text:    fmt.Sprintf("[exec read decode failed: %v]", err),
				IsError: true,
			}, nil
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
		text.WriteString(fmt.Sprintf("\n[exec failure: %s]", *failure))
	}
	if exitCode != nil {
		text.WriteString(fmt.Sprintf("\n[exit_code=%d]", *exitCode))
	} else {
		text.WriteString("\n[exec timed out without exit signal]")
	}

	isErr := failure != nil || (exitCode != nil && *exitCode != 0) || exitCode == nil
	return ShellResult{Text: text.String(), IsError: isErr}, nil
}
