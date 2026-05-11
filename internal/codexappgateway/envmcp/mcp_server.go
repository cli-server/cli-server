package envmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"unicode/utf8"
)

// ShellRunner is the slice of Translator that MCPServer uses.
// Defined as an interface so mcp_server tests don't need a real bridge.
type ShellRunner interface {
	RunShell(ctx context.Context, argv []string, cwd string) (ShellResult, error)
}

// MCPServer is a minimal newline-delimited JSON-RPC stdio MCP server
// that exposes a single `shell` tool. Concurrency: requests are handled
// sequentially in the order they arrive; this matches the MCP stdio
// model and keeps the server free of intra-process synchronization
// other than the write-mutex.
type MCPServer struct {
	exeDesc string
	tr      ShellRunner
	writeMu sync.Mutex
	logger  *slog.Logger
}

func NewMCPServer(exeDesc string, tr ShellRunner, logger *slog.Logger) *MCPServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &MCPServer{exeDesc: exeDesc, tr: tr, logger: logger}
}

// previewLine returns up to 200 bytes of line as a string, truncating safely.
func previewLine(line []byte) string {
	const max = 200
	if len(line) <= max {
		return string(line)
	}
	// Truncate at a valid UTF-8 boundary.
	truncated := line[:max]
	for !utf8.Valid(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return string(truncated) + "…"
}

// Serve reads requests from in until EOF and writes responses to out.
// Returns nil on clean EOF, error on unrecoverable read/write failure.
func (s *MCPServer) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req JSONRPCMessage
		if err := json.Unmarshal(line, &req); err != nil {
			s.logger.Warn("mcp: dropping malformed JSON-RPC line", "err", err, "preview", previewLine(line))
			continue
		}
		if err := s.dispatch(ctx, &req, out); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s *MCPServer) dispatch(ctx context.Context, req *JSONRPCMessage, out io.Writer) error {
	switch req.Method {
	case "initialize":
		return s.respond(out, req.ID, MCPInitializeResult{
			ProtocolVersion: "2025-06-18",
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      MCPServerInfo{Name: "codex-env-mcp", Version: "0.1"},
		}, nil)

	case "notifications/initialized":
		return nil // notification

	case "tools/list":
		schema := json.RawMessage(`{"type":"object","properties":{` +
			`"command":{"type":"array","items":{"type":"string"},"description":"argv as a list of strings"},` +
			`"cwd":{"type":"string","description":"Working directory; defaults to /tmp"}` +
			`},"required":["command"]}`)
		desc := fmt.Sprintf(
			"Run a shell command on `%s`. Use this tool for any shell operation in this environment.",
			s.exeDesc,
		)
		return s.respond(out, req.ID, MCPListToolsResult{
			Tools: []MCPTool{{Name: "shell", Description: desc, InputSchema: schema}},
		}, nil)

	case "tools/call":
		var p MCPCallToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return s.respond(out, req.ID, nil, &JSONRPCError{Code: -32602, Message: "invalid params: " + err.Error()})
		}
		if p.Name != "shell" {
			return s.respond(out, req.ID, nil, &JSONRPCError{Code: -32601, Message: "unknown tool: " + p.Name})
		}
		var args struct {
			Command []string `json:"command"`
			Cwd     string   `json:"cwd"`
		}
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return s.respond(out, req.ID, nil, &JSONRPCError{Code: -32602, Message: "invalid arguments: " + err.Error()})
		}
		if len(args.Command) == 0 {
			return s.respond(out, req.ID, nil, &JSONRPCError{Code: -32602, Message: "command must be a non-empty array"})
		}
		cwd := args.Cwd
		if cwd == "" {
			cwd = "/tmp"
		}
		res, err := s.tr.RunShell(ctx, args.Command, cwd)
		if err != nil {
			return s.respond(out, req.ID, nil, &JSONRPCError{Code: -32000, Message: "shell failed: " + err.Error()})
		}
		return s.respond(out, req.ID, MCPCallToolResult{
			Content: []MCPToolContent{{Type: "text", Text: res.Text}},
			IsError: res.IsError,
		}, nil)

	case "prompts/list":
		return s.respond(out, req.ID, map[string]any{"prompts": []any{}}, nil)
	case "resources/list":
		return s.respond(out, req.ID, map[string]any{"resources": []any{}}, nil)
	case "resources/templates/list":
		return s.respond(out, req.ID, map[string]any{"resourceTemplates": []any{}}, nil)

	default:
		if req.ID == nil {
			return nil // notification of unknown method — drop
		}
		return s.respond(out, req.ID, nil, &JSONRPCError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

func (s *MCPServer) respond(out io.Writer, id *int64, result any, errObj *JSONRPCError) error {
	if id == nil && errObj == nil {
		return nil // nothing to say back
	}
	msg := JSONRPCMessage{JSONRPC: "2.0", ID: id, Error: errObj}
	if errObj == nil {
		raw, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		msg.Result = raw
	}
	out2, err := json.Marshal(&msg)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := out.Write(append(out2, '\n')); err != nil {
		return errors.New("mcp write: " + err.Error())
	}
	return nil
}
