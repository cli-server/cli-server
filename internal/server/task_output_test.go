package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentserver/agentserver/internal/db"
)

func TestExtractTaskOutput_AssistantText(t *testing.T) {
	events := []db.AgentSessionEvent{
		makeEvent(`{"type":"assistant","message":{"content":[{"type":"text","text":"Here is the output of df -h:"}]}}`),
	}
	got := extractTaskOutput(events)
	if got == "" {
		t.Fatal("expected non-empty output")
	}
	want := "Here is the output of df -h:"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractTaskOutput_ToolResult(t *testing.T) {
	events := []db.AgentSessionEvent{
		makeEvent(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"Filesystem  Size  Used  Avail\n/dev/sda1   100G  40G   60G"}]}}`),
	}
	got := extractTaskOutput(events)
	if got == "" {
		t.Fatal("expected non-empty output")
	}
	if want := "Filesystem  Size  Used  Avail\n/dev/sda1   100G  40G   60G"; want != got {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractTaskOutput_Mixed(t *testing.T) {
	events := []db.AgentSessionEvent{
		makeEvent(`{"type":"assistant","message":{"content":[{"type":"text","text":"Running df -h..."},{"type":"tool_use","id":"tu_1","name":"Bash"}]}}`),
		makeEvent(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"/ 100G 40G 60G"}]}}`),
		makeEvent(`{"type":"assistant","message":{"content":[{"type":"text","text":"Disk is 40% used."}]}}`),
	}
	got := extractTaskOutput(events)
	if got == "" {
		t.Fatal("expected non-empty output")
	}
	for _, sub := range []string{"Running df -h...", "/ 100G 40G 60G", "Disk is 40% used."} {
		if !strings.Contains(got, sub) {
			t.Errorf("output missing %q, got:\n%s", sub, got)
		}
	}
}

func TestExtractTaskOutput_ToolResultArrayContent(t *testing.T) {
	events := []db.AgentSessionEvent{
		makeEvent(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}}`),
	}
	got := extractTaskOutput(events)
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("expected both lines from array content, got %q", got)
	}
}

func TestExtractTaskOutput_Empty(t *testing.T) {
	got := extractTaskOutput(nil)
	if got != "" {
		t.Errorf("expected empty string for nil events, got %q", got)
	}
}

func makeEvent(payload string) db.AgentSessionEvent {
	raw := json.RawMessage(payload)
	return db.AgentSessionEvent{
		EventType: "client_event",
		Source:    "worker",
		Payload:   raw,
	}
}
