package tools

import (
	"context"
	"encoding/json"
)

// Tool is implemented by every MCP tool env-mcp exposes. tools/list
// builds its response by querying each registered Tool's metadata;
// tools/call dispatches by Name.
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Call(ctx context.Context, args json.RawMessage) (MCPCallToolResult, error)
}

// MCPCallToolResult is the response body of `tools/call`.
type MCPCallToolResult struct {
	Content []MCPToolContent `json:"content"`
	IsError bool             `json:"isError"`
}

type MCPToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MCPInitializeResult is the response to `initialize`.
type MCPInitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      MCPServerInfo  `json:"serverInfo"`
}

type MCPServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MCPListToolsResult is the response to `tools/list`.
type MCPListToolsResult struct {
	Tools []MCPTool `json:"tools"`
}

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPCallToolParams is the request body of `tools/call`.
type MCPCallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
