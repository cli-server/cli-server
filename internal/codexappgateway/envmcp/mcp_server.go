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

	"github.com/agentserver/agentserver/internal/envtools/bridge"
	"github.com/agentserver/agentserver/internal/envtools/tools"
)

// MCPServer is a minimal newline-delimited JSON-RPC stdio MCP server
// that exposes a fixed set of tools through a registry. Concurrency:
// requests are handled sequentially in the order they arrive; this
// matches the MCP stdio model and keeps the server free of
// intra-process synchronization other than the write-mutex.
type MCPServer struct {
	name    string // surfaces in initialize/serverInfo
	tools   map[string]tools.Tool
	order   []string // stable tools/list ordering
	writeMu sync.Mutex
	logger  *slog.Logger
}

// NewMCPServer constructs a server bound to a registry. Tool order is
// preserved as supplied (LLM clients sometimes rely on consistent
// ordering for caching).
func NewMCPServer(name string, ts []tools.Tool, logger *slog.Logger) *MCPServer {
	if logger == nil {
		logger = slog.Default()
	}
	reg := make(map[string]tools.Tool, len(ts))
	order := make([]string, 0, len(ts))
	for _, t := range ts {
		if _, dup := reg[t.Name()]; dup {
			logger.Warn("mcp: duplicate tool name; later registration wins", "name", t.Name())
		}
		reg[t.Name()] = t
		order = append(order, t.Name())
	}
	return &MCPServer{name: name, tools: reg, order: order, logger: logger}
}

// previewLine returns up to 200 bytes of line as a string, truncating safely.
func previewLine(line []byte) string {
	const max = 200
	if len(line) <= max {
		return string(line)
	}
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
		var req bridge.JSONRPCMessage
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

func (s *MCPServer) dispatch(ctx context.Context, req *bridge.JSONRPCMessage, out io.Writer) error {
	switch req.Method {
	case "initialize":
		return s.respond(out, req.ID, tools.MCPInitializeResult{
			ProtocolVersion: "2025-06-18",
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      tools.MCPServerInfo{Name: s.name, Version: "0.2"},
		}, nil)

	case "notifications/initialized":
		return nil // notification

	case "tools/list":
		list := make([]tools.MCPTool, 0, len(s.order))
		for _, name := range s.order {
			t := s.tools[name]
			list = append(list, tools.MCPTool{
				Name:        t.Name(),
				Description: t.Description(),
				InputSchema: t.InputSchema(),
			})
		}
		return s.respond(out, req.ID, tools.MCPListToolsResult{Tools: list}, nil)

	case "tools/call":
		var p tools.MCPCallToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return s.respond(out, req.ID, nil, &bridge.JSONRPCError{Code: -32602, Message: "invalid params: " + err.Error()})
		}
		t, ok := s.tools[p.Name]
		if !ok {
			return s.respond(out, req.ID, nil, &bridge.JSONRPCError{Code: -32601, Message: "unknown tool: " + p.Name})
		}
		res, err := t.Call(ctx, p.Arguments)
		if err != nil {
			// Tool returned a hard error (not an isError content) — surface
			// as JSON-RPC error so the LLM sees a clear protocol failure
			// rather than a silently-empty content list.
			return s.respond(out, req.ID, nil, &bridge.JSONRPCError{Code: -32000, Message: p.Name + ": " + err.Error()})
		}
		return s.respond(out, req.ID, res, nil)

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
		return s.respond(out, req.ID, nil, &bridge.JSONRPCError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

func (s *MCPServer) respond(out io.Writer, id *int64, result any, errObj *bridge.JSONRPCError) error {
	if id == nil && errObj == nil {
		return nil // nothing to say back
	}
	msg := bridge.JSONRPCMessage{JSONRPC: "2.0", ID: id, Error: errObj}
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
