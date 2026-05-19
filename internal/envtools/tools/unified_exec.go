package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/agentserver/agentserver/internal/envtools/bridge"
	"github.com/agentserver/agentserver/internal/envtools/nameresolver"
)

// session ties an MCP-facing session_id to a remote process. The
// session_id is what we hand the LLM; processID is the exec-server
// pid; exeID is the executor it lives on.
type unifiedSession struct {
	sessionID string
	exeID     string
	processID string
	createdAt time.Time
}

// SessionStore tracks open exec_command sessions and GCs old entries
// (anything older than sessionMaxAge gets reaped on each access). The
// GC is best-effort — sessions whose underlying process exited on its
// own simply linger until access pressure prunes them.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*unifiedSession
}

const sessionMaxAge = 30 * time.Minute

// NewSessionStore creates a new empty SessionStore.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]*unifiedSession{}}
}

func (s *SessionStore) add(exeID, processID string) string {
	sid := uuid.NewString()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.sessions[sid] = &unifiedSession{sessionID: sid, exeID: exeID, processID: processID, createdAt: time.Now()}
	return sid
}

func (s *SessionStore) lookup(sessionID string) (*unifiedSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	v, ok := s.sessions[sessionID]
	return v, ok
}

func (s *SessionStore) drop(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

func (s *SessionStore) gcLocked() {
	cutoff := time.Now().Add(-sessionMaxAge)
	for k, v := range s.sessions {
		if v.createdAt.Before(cutoff) {
			delete(s.sessions, k)
		}
	}
}

// UnifiedExecTool starts a long-lived process and returns a session_id
// the LLM uses with write_stdin / read_output / terminate.
type UnifiedExecTool struct {
	pool     *bridge.Pool
	sessions *SessionStore
	resolver *nameresolver.Resolver
	pidSeq   atomic.Uint64
}

func NewUnifiedExecTool(pool *bridge.Pool, store *SessionStore, resolver *nameresolver.Resolver) *UnifiedExecTool {
	return &UnifiedExecTool{pool: pool, sessions: store, resolver: resolver}
}

var unifiedExecSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "environment_id": {"type": "string", "description": "Target environment's exe_id (from list_environments output). NOT the description."},
    "command": {"type": "array", "items": {"type": "string"}},
    "cwd": {"type": "string"},
    "tty": {"type": "boolean", "description": "Allocate a PTY"},
    "pipe_stdin": {"type": "boolean", "description": "Open stdin pipe (required if you intend to call write_stdin)"}
  },
  "required": ["environment_id", "command"]
}`)

func (t *UnifiedExecTool) Name() string { return "exec_command" }

func (t *UnifiedExecTool) Description() string {
	return "Start a long-lived process on the named environment and return a session_id. " +
		"Use write_stdin to feed input, read_output to drain stdout/stderr, terminate to stop. " +
		"For one-shot commands prefer `shell`."
}

func (t *UnifiedExecTool) InputSchema() json.RawMessage { return unifiedExecSchema }

func (t *UnifiedExecTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvironmentID string   `json:"environment_id"`
		Command   []string `json:"command"`
		Cwd       string   `json:"cwd"`
		TTY       bool     `json:"tty"`
		PipeStdin bool     `json:"pipe_stdin"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if a.EnvironmentID == "" || len(a.Command) == 0 {
		return errResult("environment_id and command are required"), nil
	}
	if a.Cwd == "" {
		a.Cwd = "/tmp"
	}
	exeID, err := t.resolver.Resolve(ctx, a.EnvironmentID)
	if err != nil {
		return errResult(err.Error()), nil
	}
	bc, err := t.pool.Get(ctx, exeID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q unavailable: %v", a.EnvironmentID, err)), nil
	}
	pid := fmt.Sprintf("uexec-%d", t.pidSeq.Add(1))
	startParams, _ := json.Marshal(bridge.ProcessStartParams{
		ProcessID: pid,
		Argv:      a.Command,
		Cwd:       a.Cwd,
		Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
		TTY:       a.TTY,
		PipeStdin: a.PipeStdin,
	})
	if _, err := bc.Call(ctx, bridge.ExecMethodProcessStart, startParams); err != nil {
		return errResult(fmt.Sprintf("[exec failed to start: %v]", err)), nil
	}
	// Session stores the resolved exe_id so subsequent write_stdin/
	// read_output/terminate don't need to re-resolve the name.
	sid := t.sessions.add(exeID, pid)
	body, _ := json.Marshal(map[string]string{"session_id": sid})
	return MCPCallToolResult{Content: []MCPToolContent{{Type: "text", Text: string(body)}}}, nil
}

// WriteStdinTool writes bytes to a session's stdin via process/write.
type WriteStdinTool struct {
	pool     *bridge.Pool
	sessions *SessionStore
}

func NewWriteStdinTool(pool *bridge.Pool, store *SessionStore) *WriteStdinTool {
	return &WriteStdinTool{pool: pool, sessions: store}
}

var writeStdinSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "environment_id": {"type": "string", "description": "Target environment's exe_id (from list_environments output). NOT the description."},
    "session_id": {"type": "string"},
    "chars": {"type": "string", "description": "Text written to stdin. Trailing newlines must be included explicitly."}
  },
  "required": ["environment_id", "session_id", "chars"]
}`)

func (t *WriteStdinTool) Name() string                  { return "write_stdin" }
func (t *WriteStdinTool) InputSchema() json.RawMessage  { return writeStdinSchema }
func (t *WriteStdinTool) Description() string {
	return "Write data to the stdin of an exec_command session. The session must have been " +
		"started with pipe_stdin=true."
}

func (t *WriteStdinTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvironmentID string `json:"environment_id"`
		SessionID string `json:"session_id"`
		Chars     string `json:"chars"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	sess, ok := t.sessions.lookup(a.SessionID)
	if !ok || sess.exeID != a.EnvironmentID {
		return errResult("no such session for that environment_id"), nil
	}
	bc, err := t.pool.Get(ctx, sess.exeID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q unavailable: %v", sess.exeID, err)), nil
	}
	params, _ := json.Marshal(bridge.ProcessWriteParams{
		ProcessID: sess.processID,
		Chunk:     base64.StdEncoding.EncodeToString([]byte(a.Chars)),
	})
	if _, err := bc.Call(ctx, bridge.ExecMethodProcessWrite, params); err != nil {
		return errResult(fmt.Sprintf("write failed: %v", err)), nil
	}
	return MCPCallToolResult{Content: []MCPToolContent{{Type: "text", Text: "ok"}}}, nil
}

// ReadOutputTool drains stdout/stderr buffered for a session.
type ReadOutputTool struct {
	pool     *bridge.Pool
	sessions *SessionStore
}

func NewReadOutputTool(pool *bridge.Pool, store *SessionStore) *ReadOutputTool {
	return &ReadOutputTool{pool: pool, sessions: store}
}

var readOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "environment_id": {"type": "string", "description": "Target environment's exe_id (from list_environments output). NOT the description."},
    "session_id": {"type": "string"},
    "after_seq": {"type": "integer", "description": "Skip output up to this seq number (returned by previous read)"},
    "yield_time_ms": {"type": "integer", "description": "How long to block waiting for new output; default 1000"}
  },
  "required": ["environment_id", "session_id"]
}`)

func (t *ReadOutputTool) Name() string                  { return "read_output" }
func (t *ReadOutputTool) InputSchema() json.RawMessage  { return readOutputSchema }
func (t *ReadOutputTool) Description() string {
	return "Read accumulated output from an exec_command session. Returns chunks + next_seq " +
		"for the next read, plus exited/exit_code if the process has finished."
}

func (t *ReadOutputTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvironmentID string `json:"environment_id"`
		SessionID string `json:"session_id"`
		AfterSeq  uint64 `json:"after_seq"`
		YieldTimeMs int `json:"yield_time_ms"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	sess, ok := t.sessions.lookup(a.SessionID)
	if !ok || sess.exeID != a.EnvironmentID {
		return errResult("no such session for that environment_id"), nil
	}
	if a.YieldTimeMs <= 0 {
		a.YieldTimeMs = 1000
	}
	bc, err := t.pool.Get(ctx, sess.exeID)
	if err != nil {
		return errResult(fmt.Sprintf("environment %q unavailable: %v", sess.exeID, err)), nil
	}
	params, _ := json.Marshal(bridge.ProcessReadParams{
		ProcessID: sess.processID, AfterSeq: a.AfterSeq,
		MaxBytes: defaultMaxBytes, WaitMs: a.YieldTimeMs,
	})
	rawResp, err := bc.Call(ctx, bridge.ExecMethodProcessRead, params)
	if err != nil {
		return errResult(fmt.Sprintf("read failed: %v", err)), nil
	}
	// Pass the result through to the LLM as JSON text so it can decide
	// when to keep polling vs stop.
	return MCPCallToolResult{Content: []MCPToolContent{{Type: "text", Text: string(rawResp)}}}, nil
}

// TerminateTool sends process/terminate then drops the session entry.
type TerminateTool struct {
	pool     *bridge.Pool
	sessions *SessionStore
}

func NewTerminateTool(pool *bridge.Pool, store *SessionStore) *TerminateTool {
	return &TerminateTool{pool: pool, sessions: store}
}

var terminateSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "environment_id": {"type": "string", "description": "Target environment's exe_id (from list_environments output). NOT the description."},
    "session_id": {"type": "string"}
  },
  "required": ["environment_id", "session_id"]
}`)

func (t *TerminateTool) Name() string                  { return "terminate" }
func (t *TerminateTool) InputSchema() json.RawMessage  { return terminateSchema }
func (t *TerminateTool) Description() string {
	return "Terminate an exec_command session and release its resources."
}

func (t *TerminateTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a struct {
		EnvironmentID string `json:"environment_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	sess, ok := t.sessions.lookup(a.SessionID)
	if !ok || sess.exeID != a.EnvironmentID {
		return errResult("no such session for that environment_id"), nil
	}
	t.sessions.drop(a.SessionID)
	bc, err := t.pool.Get(ctx, sess.exeID)
	if err != nil {
		// Session already gone — succeed quietly.
		return MCPCallToolResult{Content: []MCPToolContent{{Type: "text", Text: "terminated"}}}, nil
	}
	params, _ := json.Marshal(bridge.ProcessTerminateParams{ProcessID: sess.processID})
	if _, err := bc.Call(ctx, bridge.ExecMethodProcessTerminate, params); err != nil {
		return errResult(fmt.Sprintf("terminate failed: %v", err)), nil
	}
	return MCPCallToolResult{Content: []MCPToolContent{{Type: "text", Text: "terminated"}}}, nil
}
