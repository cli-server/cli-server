package oplog

import (
	"context"
	"encoding/json"
	"fmt"
)

// TryHandleOperationsList inspects a client→server frame. If it's an
// `operations/list` JSON-RPC request, it forwards the filters to
// agentserver via the ListClient and returns a complete JSON-RPC response
// frame to send back to the client (ok=true). For any other frame,
// returns (nil, false) — caller forwards the frame normally.
//
// Designed to be called from the gateway's outbound proxy path so the
// request never reaches the codex app-server (which has no operations/list
// method).
func TryHandleOperationsList(
	ctx context.Context,
	lc *ListClient,
	workspaceID string,
	frame []byte,
) ([]byte, bool) {
	var msg struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		return nil, false
	}
	if msg.Method != "operations/list" || msg.ID == nil {
		return nil, false
	}

	params := map[string]string{"workspace_id": workspaceID}
	var p struct {
		Limit   *int    `json:"limit"`
		EnvID   *string `json:"env_id"`
		Tool    *string `json:"tool"`
		Source  *string `json:"source"`
		IsError *bool   `json:"is_error"`
		Since   *string `json:"since"`
		ID      *string `json:"id"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if p.Limit != nil {
		params["limit"] = fmt.Sprintf("%d", *p.Limit)
	}
	if p.EnvID != nil {
		params["env_id"] = *p.EnvID
	}
	if p.Tool != nil {
		params["tool"] = *p.Tool
	}
	if p.Source != nil {
		params["source"] = *p.Source
	}
	if p.IsError != nil {
		params["is_error"] = fmt.Sprintf("%t", *p.IsError)
	}
	if p.Since != nil {
		params["since"] = *p.Since
	}
	if p.ID != nil {
		params["id"] = *p.ID
	}

	body, err := lc.List(ctx, params)
	if err != nil {
		errResp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": msg.ID,
			"error": map[string]any{"code": -32603, "message": err.Error()},
		})
		return errResp, true
	}
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": msg.ID,
		"result": json.RawMessage(body),
	})
	return resp, true
}
