// Package broker is a thin REST→ws codex v2 JSON-RPC adapter inside CXG.
// It owns no business logic: it converts a single /api/turns REST call
// into a turn lifecycle on a loopback ws to a codex app-server
// subprocess, returning the resulting codex Turn object verbatim.
package broker

import (
	"encoding/json"
)

// --- JSON-RPC envelopes (codex uses 2.0 but tolerates omission) ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc,omitempty"` // "2.0" — codex tolerates omission, we include
	ID      *int64          `json:"id,omitempty"`      // nil = notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	// Notification methods (ID nil) carry Method + Params instead.
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// --- thread/* (we only construct minimal payloads) ---

// ThreadStartParams: empty {} suffices for MVP — codex defaults the rest.
// All fields optional per v2/thread.rs:ThreadStartParams.
type threadStartParams struct{}

// ThreadStartResponse: we only need thread.id.
type threadStartResponse struct {
	Thread thread `json:"thread"`
}

type thread struct {
	ID string `json:"id"`
}

// --- turn/start ---

// TurnStartParams. We pass through caller's input verbatim via RawMessage
// so codex schema growth (model overrides, environments) doesn't require
// changes here. ThreadID is set by us, not the caller.
type turnStartParams struct {
	ThreadID string          `json:"threadId"`
	Input    json.RawMessage `json:"input"`
}

// TurnStartResponse: codex returns {turn: Turn}. We need turn.id to
// match later TurnCompleted notifications.
type turnStartResponse struct {
	Turn turnRef `json:"turn"`
}

type turnRef struct {
	ID string `json:"id"`
}

// --- turn/interrupt ---

type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// --- Notifications we listen for ---

// TurnCompletedNotification.params shape (v2/turn.rs:329).
// The full Turn object is opaque to us — we hand it back to the REST
// caller as-is. We only peek threadId/turn.id for routing.
type turnCompletedParams struct {
	ThreadID string      `json:"threadId"`
	Turn     turnPayload `json:"turn"`
}

// turnPayload exposes only the routing key. The full object is in Raw
// for verbatim REST passthrough.
type turnPayload struct {
	ID  string          `json:"id"`
	Raw json.RawMessage `json:"-"` // populated by custom UnmarshalJSON
}

func (t *turnPayload) UnmarshalJSON(data []byte) error {
	t.Raw = append(t.Raw[:0], data...)
	var shell struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &shell); err != nil {
		return err
	}
	t.ID = shell.ID
	return nil
}

// --- Approval frames we auto-reply to ---

// Methods listed at codex-rs/app-server-protocol/src/protocol/common.rs:1277.
// We don't unmarshal params; we just need the request id to reply.
const (
	methodItemCmdApproval   = "item/commandExecution/requestApproval"
	methodItemFileApproval  = "item/fileChange/requestApproval"
	methodItemUserInput     = "item/tool/requestUserInput"
	methodItemPermsApproval = "item/permissions/requestApproval"
	methodMcpElicitation    = "mcpServer/elicitation/request"
)

func isApprovalRequest(method string) bool {
	switch method {
	case methodItemCmdApproval, methodItemFileApproval, methodItemUserInput,
		methodItemPermsApproval, methodMcpElicitation:
		return true
	}
	return false
}

// approvalReply returns the JSON we send back. Codex's decision enums
// differ per request type but all accept "approve"/"allow" shapes —
// per CommandExecutionApprovalDecision, FileChangeApprovalDecision,
// PermissionsApprovalDecision (all carry "approve" or equivalent).
// requestUserInput is generic; codex accepts {} or {"decision":"allow"}.
func approvalReply(method string) json.RawMessage {
	switch method {
	case methodItemPermsApproval:
		return json.RawMessage(`{"decision":"allow"}`)
	default:
		return json.RawMessage(`{"decision":"approve"}`)
	}
}
