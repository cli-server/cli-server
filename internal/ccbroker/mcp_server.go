package ccbroker

import (
	"context"
	"encoding/json"
	"net/http"
)

// JSON-RPC 2.0 types

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  interface{}      `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Standard JSON-RPC 2.0 error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

// MCP types

type MCPToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"inputSchema"`
}

type MCPToolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

type MCPContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type MCPToolResult struct {
	Content []MCPContentBlock `json:"content"`
	IsError bool              `json:"isError,omitempty"`
}

// Stub ToolRouter — will be replaced in Task 3.
type ToolRouter struct{}

func (r *ToolRouter) Route(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolResult, error) {
	return &MCPToolResult{Content: []MCPContentBlock{{Type: "text", Text: "not implemented"}}}, nil
}

// MCPServer implements http.Handler and speaks JSON-RPC 2.0 / MCP.
type MCPServer struct {
	router *ToolRouter
}

func NewMCPServer(router *ToolRouter) *MCPServer {
	return &MCPServer{router: router}
}

func (s *MCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   &RPCError{Code: rpcParseError, Message: "parse error: " + err.Error()},
		})
		return
	}

	if req.JSONRPC != "2.0" {
		writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: rpcInvalidRequest, Message: "invalid JSON-RPC version"},
		})
		return
	}

	// Notifications (no id) that require no response body.
	if req.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, r, &req)
	case "tools/list":
		s.handleToolsList(w, r, &req)
	case "tools/call":
		s.handleToolsCall(w, r, &req)
	default:
		writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: rpcMethodNotFound, Message: "method not found: " + req.Method},
		})
	}
}

func (s *MCPServer) handleInitialize(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	result := map[string]interface{}{
		"protocolVersion": "2025-03-26",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "cc-broker",
			"version": "0.1.0",
		},
	}
	writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	})
}

func (s *MCPServer) handleToolsList(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	tools := buildToolDefinitions()
	result := map[string]interface{}{
		"tools": tools,
	}
	writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	})
}

func (s *MCPServer) handleToolsCall(w http.ResponseWriter, r *http.Request, req *JSONRPCRequest) {
	var params MCPToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: rpcInvalidParams, Message: "invalid params: " + err.Error()},
		})
		return
	}

	if params.Name == "" {
		writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: rpcInvalidParams, Message: "params.name is required"},
		})
		return
	}

	result, err := s.router.Route(r.Context(), params.Name, params.Arguments)
	if err != nil {
		writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &RPCError{Code: rpcInternalError, Message: err.Error()},
		})
		return
	}

	writeJSONRPC(w, http.StatusOK, &JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	})
}

// writeJSONRPC writes a JSON-RPC 2.0 response with the given HTTP status code.
func writeJSONRPC(w http.ResponseWriter, status int, resp *JSONRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}
