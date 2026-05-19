package broker

import (
	"encoding/json"
	"testing"
)

func TestRPCRequestMarshalsNotification(t *testing.T) {
	r := rpcRequest{
		JSONRPC: "2.0",
		Method:  "initialized",
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"jsonrpc":"2.0","method":"initialized"}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestRPCRequestMarshalsCall(t *testing.T) {
	id := int64(7)
	r := rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "thread/start",
		Params:  json.RawMessage(`{}`),
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"jsonrpc":"2.0","id":7,"method":"thread/start","params":{}}` {
		t.Errorf("got %s", b)
	}
}

func TestTurnCompletedParamsKeepsRawTurn(t *testing.T) {
	frame := []byte(`{"threadId":"thr-1","turn":{"id":"trn-9","status":"completed","items":[{"type":"agentMessage","id":"msg-1","text":"hi"}],"itemsView":"full","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}`)
	var p turnCompletedParams
	if err := json.Unmarshal(frame, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ThreadID != "thr-1" {
		t.Errorf("threadID=%q", p.ThreadID)
	}
	if p.Turn.ID != "trn-9" {
		t.Errorf("turn.id=%q", p.Turn.ID)
	}
	// The raw Turn payload must be preserved verbatim for REST passthrough.
	var rt map[string]any
	if err := json.Unmarshal(p.Turn.Raw, &rt); err != nil {
		t.Fatalf("turn.raw unmarshal: %v", err)
	}
	if rt["status"] != "completed" {
		t.Errorf("raw turn lost: %v", rt)
	}
	items, _ := rt["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items lost: %v", items)
	}
}

func TestIsApprovalRequest(t *testing.T) {
	cases := map[string]bool{
		"item/commandExecution/requestApproval": true,
		"item/fileChange/requestApproval":       true,
		"item/tool/requestUserInput":            true,
		"item/permissions/requestApproval":      true,
		"mcpServer/elicitation/request":         true,
		"turn/started":                          false,
		"turn/completed":                        false,
		"item/completed":                        false,
	}
	for m, want := range cases {
		if got := isApprovalRequest(m); got != want {
			t.Errorf("%s: got %v want %v", m, got, want)
		}
	}
}

func TestApprovalReplyShapes(t *testing.T) {
	cases := []struct {
		method string
		want   string
	}{
		{methodItemCmdApproval, `{"decision":"accept"}`},
		{methodItemFileApproval, `{"decision":"accept"}`},
		{methodItemPermsApproval, `{"permissions":{}}`},
		{methodItemUserInput, `{"answers":{}}`},
		{methodMcpElicitation, `{"action":"accept","content":null,"_meta":null}`},
		{"unknown/method", `{}`},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			got := string(approvalReply(c.method))
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
