// Package approvalfilter synthesizes auto-accept responses for codex
// app-server's server-to-client approval/elicitation requests.
//
// Codex pushes 5 kinds of approval-style requests at the gateway during
// a turn (item/commandExecution/requestApproval, etc.). Without a
// response, the turn stalls. Codex is configured with
// default_tools_approval_mode = "approve" so these requests rarely
// fire — but when they do, this package returns the schema-valid
// auto-accept payload.
//
// Payloads track upstream codex schemas at the tag pinned in
// codex-pin.json. The pin's CI lint scans upstream for new approval
// methods on each bump; if a new method appears, add it here.
package approvalfilter

import (
	"encoding/json"
)

const (
	methodItemCmdApproval   = "item/commandExecution/requestApproval"
	methodItemFileApproval  = "item/fileChange/requestApproval"
	methodItemPermsApproval = "item/permissions/requestApproval"
	methodItemUserInput     = "item/tool/requestUserInput"
	methodMcpElicitation    = "mcpServer/elicitation/request"
)

// Methods returns the exhaustive list of approval method names.
// Used by codex-pin CI lint and by integration tests.
func Methods() []string {
	return []string{
		methodItemCmdApproval,
		methodItemFileApproval,
		methodItemPermsApproval,
		methodItemUserInput,
		methodMcpElicitation,
	}
}

// IsApproval reports whether method is an approval-style server-to-client
// request that needs a synthesized reply.
func IsApproval(method string) bool {
	switch method {
	case methodItemCmdApproval, methodItemFileApproval, methodItemPermsApproval,
		methodItemUserInput, methodMcpElicitation:
		return true
	}
	return false
}

// Reply returns the JSON-RPC `result` payload for a known approval method.
// For commandExecution/fileChange we use {"decision":"accept"} (most
// permissive variant of the enum). For permissions we send
// {"permissions":{}} (no extra grants). For requestUserInput we send no
// answers. For mcpServer/elicitation we send action:"accept" with null
// content. For unknown methods, returns "{}" as a defensive default.
//
// Payload shapes match codex v2 enum/struct definitions in
// app-server-protocol/src/protocol/v2/ at the pinned tag.
func Reply(method string) json.RawMessage {
	switch method {
	case methodItemCmdApproval, methodItemFileApproval:
		return json.RawMessage(`{"decision":"accept"}`)
	case methodItemPermsApproval:
		return json.RawMessage(`{"permissions":{}}`)
	case methodItemUserInput:
		return json.RawMessage(`{"answers":{}}`)
	case methodMcpElicitation:
		return json.RawMessage(`{"action":"accept","content":null,"_meta":null}`)
	}
	return json.RawMessage(`{}`)
}

// TryReply inspects a server-to-client JSON-RPC frame. If it's an
// approval request (recognised method + present id), returns the
// complete {jsonrpc, id, result} response bytes ready to write back to
// upstream, along with true. Otherwise returns nil, false.
//
// Never blocks; never errors. Malformed frames return nil, false.
func TryReply(frame []byte) ([]byte, bool) {
	var f struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		return nil, false
	}
	if !IsApproval(f.Method) {
		return nil, false
	}
	if len(f.ID) == 0 || string(f.ID) == "null" {
		// Server-sent notification (no id) — can't reply.
		return nil, false
	}
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      f.ID,
		Result:  Reply(f.Method),
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, false
	}
	return b, true
}
