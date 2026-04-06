// Package mcpbridge implements a stdio MCP server that exposes agentserver
// agent discovery and task delegation as MCP tools for Claude Code.
//
// Protocol: JSON-RPC 2.0 over stdin/stdout (newline-delimited).
// Methods: initialize, notifications/initialized, tools/list, tools/call.
package mcpbridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// Request is a JSON-RPC 2.0 request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent for notifications; "null" is valid request id
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification returns true if this is a notification (no id field).
func (r *Request) IsNotification() bool {
	return len(r.ID) == 0
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server handles JSON-RPC 2.0 messages on stdin/stdout.
type Server struct {
	tools   []ToolDef
	handler func(name string, args json.RawMessage) (*ToolResult, error)
	mu      sync.Mutex
	writer  io.Writer
}

// NewServer creates an MCP stdio server.
func NewServer(tools []ToolDef, handler func(string, json.RawMessage) (*ToolResult, error)) *Server {
	return &Server{
		tools:   tools,
		handler: handler,
		writer:  os.Stdout,
	}
}

// Run reads requests from stdin and writes responses to stdout. Blocks until EOF.
func (s *Server) Run() error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20) // 1MB max

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("mcp: invalid json: %v", err)
			continue
		}

		resp := s.handle(req)
		if resp != nil {
			s.send(resp)
		}
	}
	return scanner.Err()
}

func (s *Server) handle(req Request) *Response {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "notifications/initialized":
		return nil // notification, no response
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(req)
	default:
		if req.IsNotification() {
			return nil // notification — no response permitted per JSON-RPC 2.0 §4
		}
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &Error{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}
	}
}

func (s *Server) handleInitialize(req Request) *Response {
	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "agentserver-mcp-bridge",
				"version": "0.1.0",
			},
		},
	}
}

func (s *Server) handleToolsList(req Request) *Response {
	tools := make([]map[string]any, len(s.tools))
	for i, t := range s.tools {
		tool := map[string]any{
			"name":        t.Name,
			"description": t.Description(),
			"inputSchema": t.InputSchema,
		}
		if t.Annotations != nil {
			tool["annotations"] = t.Annotations
		}
		tools[i] = tool
	}
	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]any{"tools": tools},
	}
}

func (s *Server) handleToolsCall(req Request) *Response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &Error{Code: -32602, Message: "invalid params"},
		}
	}

	result, err := s.handler(params.Name, params.Arguments)
	if err != nil {
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []map[string]string{{"type": "text", "text": err.Error()}},
				"isError": true,
			},
		}
	}
	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
}

func (s *Server) send(resp *Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("mcp: marshal error: %v", err)
		return
	}
	data = append(data, '\n')
	s.writer.Write(data)
}
