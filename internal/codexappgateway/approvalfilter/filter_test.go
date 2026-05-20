package approvalfilter

import (
	"encoding/json"
	"testing"
)

func TestMethods_ListsAllFive(t *testing.T) {
	got := Methods()
	want := []string{
		"item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
		"item/tool/requestUserInput",
		"mcpServer/elicitation/request",
	}
	if len(got) != len(want) {
		t.Fatalf("Methods() len: got %d, want %d", len(got), len(want))
	}
	got1 := map[string]bool{}
	for _, m := range got {
		got1[m] = true
	}
	for _, m := range want {
		if !got1[m] {
			t.Errorf("Methods() missing %q", m)
		}
	}
}

func TestReply_KnownMethods(t *testing.T) {
	cases := []struct {
		method string
		want   string
	}{
		{"item/commandExecution/requestApproval", `{"decision":"accept"}`},
		{"item/fileChange/requestApproval", `{"decision":"accept"}`},
		{"item/permissions/requestApproval", `{"permissions":{}}`},
		{"item/tool/requestUserInput", `{"answers":{}}`},
		{"mcpServer/elicitation/request", `{"action":"accept","content":null,"_meta":null}`},
	}
	for _, tc := range cases {
		got := Reply(tc.method)
		if string(got) != tc.want {
			t.Errorf("Reply(%q): got %s, want %s", tc.method, got, tc.want)
		}
	}
}

func TestReply_UnknownMethod(t *testing.T) {
	got := Reply("item/somethingNew/requestApproval")
	if string(got) != `{}` {
		t.Errorf("Reply(unknown): got %s, want {}", got)
	}
}

func TestTryReply_ApprovalRequest(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":42,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	resp, ok := TryReply(frame)
	if !ok {
		t.Fatal("TryReply: got isApproval=false, want true")
	}
	var got struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("response not valid JSON: %v\nresp=%s", err, resp)
	}
	if got.JSONRPC != "2.0" {
		t.Errorf("jsonrpc: got %q, want 2.0", got.JSONRPC)
	}
	if got.ID != 42 {
		t.Errorf("id: got %d, want 42", got.ID)
	}
	if string(got.Result) != `{"decision":"accept"}` {
		t.Errorf("result: got %s, want {\"decision\":\"accept\"}", got.Result)
	}
}

func TestTryReply_NonApprovalFrame(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{}}`)
	resp, ok := TryReply(frame)
	if ok {
		t.Errorf("TryReply on notification: got isApproval=true, want false (resp=%s)", resp)
	}
}

func TestTryReply_ApprovalWithoutID(t *testing.T) {
	// Approval request without id is malformed — treat as non-approval (can't reply).
	frame := []byte(`{"jsonrpc":"2.0","method":"item/commandExecution/requestApproval","params":{}}`)
	_, ok := TryReply(frame)
	if ok {
		t.Errorf("TryReply on approval-without-id: got isApproval=true, want false")
	}
}

func TestTryReply_MalformedJSON(t *testing.T) {
	frame := []byte(`{not valid json`)
	_, ok := TryReply(frame)
	if ok {
		t.Errorf("TryReply on malformed JSON: got isApproval=true, want false")
	}
}

func TestTryReply_PreservesStringID(t *testing.T) {
	// JSON-RPC ids can be strings, numbers, or null. Codex uses numbers in practice,
	// but the spec allows strings. The synthesized response must echo the id back
	// verbatim regardless of type.
	frame := []byte(`{"jsonrpc":"2.0","id":"abc-123","method":"item/fileChange/requestApproval","params":{}}`)
	resp, ok := TryReply(frame)
	if !ok {
		t.Fatal("TryReply: got isApproval=false, want true")
	}
	if !contains(resp, []byte(`"id":"abc-123"`)) {
		t.Errorf("response should preserve string id: %s", resp)
	}
}

func contains(haystack, needle []byte) bool {
	return len(haystack) >= len(needle) && byteContains(haystack, needle)
}

func byteContains(s, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
