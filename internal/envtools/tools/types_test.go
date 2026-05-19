package tools

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/agentserver/agentserver/internal/envtools/bridge"
)

func TestProcessOutputChunk_DecodesBase64Stream(t *testing.T) {
	raw := []byte(`{"seq":7,"stream":"stdout","chunk":"aGVsbG8="}`)
	var c bridge.ProcessOutputChunk
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Seq != 7 || c.Stream != "stdout" {
		t.Errorf("seq=%d stream=%q", c.Seq, c.Stream)
	}
	got, err := base64.StdEncoding.DecodeString(c.Chunk)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("decoded = %q", got)
	}
}

func TestExecServerMethods_Constants(t *testing.T) {
	if bridge.ExecMethodInitialize != "initialize" ||
		bridge.ExecMethodProcessStart != "process/start" ||
		bridge.ExecMethodProcessRead != "process/read" {
		t.Fatalf("method constants drifted: %s/%s/%s",
			bridge.ExecMethodInitialize, bridge.ExecMethodProcessStart, bridge.ExecMethodProcessRead)
	}
}

func TestMCPCallToolResultMarshal(t *testing.T) {
	r := MCPCallToolResult{
		Content: []MCPToolContent{{Type: "text", Text: "ok"}},
		IsError: true,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"content":[{"type":"text","text":"ok"}],"isError":true}` {
		t.Fatalf("unexpected JSON: %s", b)
	}
}
