package bridge

import "encoding/json"

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

// --- exec-server wire types (subset env-mcp uses) ---

// Method names — must match codex-rs/exec-server/src/protocol.rs.
const (
	ExecMethodInitialize       = "initialize"
	ExecMethodInitialized      = "initialized" // notification
	ExecMethodProcessStart     = "process/start"
	ExecMethodProcessRead      = "process/read"
	ExecMethodProcessWrite     = "process/write"
	ExecMethodProcessTerminate = "process/terminate"
	ExecMethodProcessExited    = "process/exited" // notification (informational; we poll instead)
	ExecMethodProcessClosed    = "process/closed" // notification (informational)
	ExecMethodFsReadFile       = "fs/readFile"
	ExecMethodFsWriteFile      = "fs/writeFile"
	ExecMethodFsRemove         = "fs/remove"
	ExecMethodFsCopy           = "fs/copy"
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
	// Cwd: when empty, omit from the wire so exec-server inherits its
	// own working directory. Sending "" was treated by Windows
	// exec-server as a literal (invalid) path → os error 267
	// "目录名称无效".
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TTY       bool              `json:"tty"`
	PipeStdin bool              `json:"pipeStdin"`
	Arg0      *string           `json:"arg0,omitempty"`
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

// ProcessWriteParams is the request body for process/write. The
// `chunk` field name matches upstream codex's WriteParams (see
// codex-rs/exec-server/src/protocol.rs); writing with `data` here
// would 400 with "missing field `chunk`".
type ProcessWriteParams struct {
	ProcessID string `json:"processId"`
	Chunk     string `json:"chunk"` // base64 raw bytes
}

// ProcessTerminateParams is the request body for process/terminate.
type ProcessTerminateParams struct {
	ProcessID string `json:"processId"`
}

// FsReadFileParams is the request body for fs/readFile.
type FsReadFileParams struct {
	Path string `json:"path"`
}

// FsReadFileResult: dataBase64 is the file's full content
// (codex returns the entire file; we expose offset/limit slicing
// in the MCP tool wrapper).
type FsReadFileResult struct {
	DataBase64 string `json:"dataBase64"`
}

// FsWriteFileParams is the request body for fs/writeFile.
type FsWriteFileParams struct {
	Path       string `json:"path"`
	DataBase64 string `json:"dataBase64"`
	// CreateMissing controls whether intermediate directories are
	// created. Codex's default is true.
	CreateMissing bool `json:"createMissing,omitempty"`
}

// FsRemoveParams is the request body for fs/remove.
type FsRemoveParams struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// FsCopyParams is the request body for fs/copy.
type FsCopyParams struct {
	SourcePath      string `json:"sourcePath"`
	DestinationPath string `json:"destinationPath"`
	Recursive       bool   `json:"recursive,omitempty"`
}
