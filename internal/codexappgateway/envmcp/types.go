package envmcp

import "encoding/json"

// --- MCP wire types (subset implemented) ---

// JSONRPCMessage is the JSON-RPC 2.0 envelope shared by both MCP (over stdio)
// and exec-server (over ws). The ID field is a pointer so notifications
// (which have no ID) marshal cleanly without the field.
type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
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

// MCPCallToolResult is the response body of `tools/call`.
type MCPCallToolResult struct {
	Content []MCPToolContent `json:"content"`
	IsError bool             `json:"isError"`
}

type MCPToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// --- exec-server wire types (subset env-mcp uses) ---

// Method names — must match codex-rs/exec-server/src/protocol.rs.
const (
	ExecMethodInitialize    = "initialize"
	ExecMethodInitialized   = "initialized"    // notification
	ExecMethodProcessStart  = "process/start"
	ExecMethodProcessRead   = "process/read"
	ExecMethodProcessExited = "process/exited" // notification (informational; we poll instead)
	ExecMethodProcessClosed = "process/closed" // notification (informational)
)

// ExecInitializeParams matches codex-rs's InitializeParams (camelCase).
type ExecInitializeParams struct {
	ClientName      string  `json:"clientName"`
	ResumeSessionID *string `json:"resumeSessionId,omitempty"`
}

type ExecInitializeResult struct {
	SessionID string `json:"sessionId"`
}

type ProcessStartParams struct {
	ProcessID string            `json:"processId"`
	Argv      []string          `json:"argv"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	TTY       bool              `json:"tty"`
	PipeStdin bool              `json:"pipeStdin"`
	Arg0      *string           `json:"arg0"`
}

type ProcessStartResult struct {
	ProcessID string `json:"processId"`
}

type ProcessReadParams struct {
	ProcessID string `json:"processId"`
	AfterSeq  uint64 `json:"afterSeq"`
	MaxBytes  int    `json:"maxBytes"`
	WaitMs    int    `json:"waitMs"`
}

type ProcessReadResult struct {
	Chunks   []ProcessOutputChunk `json:"chunks"`
	NextSeq  uint64               `json:"nextSeq"`
	Exited   bool                 `json:"exited"`
	ExitCode *int                 `json:"exitCode"`
	Closed   bool                 `json:"closed"`
	Failure  *string              `json:"failure"`
}

// ProcessOutputChunk: chunk is base64-encoded raw bytes (per codex's
// ByteChunk wrapper that uses serde_with for base64 encoding).
type ProcessOutputChunk struct {
	Seq    uint64 `json:"seq"`
	Stream string `json:"stream"` // "stdout" | "stderr"
	Chunk  string `json:"chunk"`
}
