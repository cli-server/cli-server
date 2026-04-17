package server

import (
	"strings"
	"testing"
)

func TestExtractFinalText_AssistantBlocks(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"event_type":"system","payload":{"type":"system","subtype":"init"}}`,
		`data: {"event_type":"assistant","payload":{"type":"assistant","message":{"content":[{"type":"text","text":"hello "}]}}}`,
		`data: {"event_type":"assistant","payload":{"type":"assistant","message":{"content":[{"type":"tool_use","name":"remote_bash"},{"type":"text","text":"world"}]}}}`,
		`data: {"event_type":"done"}`,
		``,
	}, "\n\n")

	got := extractFinalText(strings.NewReader(stream))
	if got != "world" {
		t.Fatalf("expected 'world', got %q", got)
	}
}

func TestExtractFinalText_ResultPreferredOverAssistant(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"event_type":"assistant","payload":{"type":"assistant","message":{"content":[{"type":"text","text":"intermediate"}]}}}`,
		`data: {"event_type":"result","payload":{"type":"result","subtype":"success","result":"final synthesized answer"}}`,
		`data: {"event_type":"done"}`,
		``,
	}, "\n\n")

	got := extractFinalText(strings.NewReader(stream))
	if got != "final synthesized answer" {
		t.Fatalf("expected result to win, got %q", got)
	}
}

func TestExtractFinalText_MultipleTextBlocks(t *testing.T) {
	stream := `data: {"event_type":"assistant","payload":{"type":"assistant","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"text","text":"c"}]}}}` + "\n\n" +
		`data: {"event_type":"done"}` + "\n\n"

	got := extractFinalText(strings.NewReader(stream))
	if got != "abc" {
		t.Fatalf("expected concat 'abc', got %q", got)
	}
}

func TestExtractFinalText_EmptyStream(t *testing.T) {
	got := extractFinalText(strings.NewReader(""))
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestExtractFinalText_NoAssistantMessage(t *testing.T) {
	stream := `data: {"event_type":"system","payload":{"type":"system"}}` + "\n\n" +
		`data: {"event_type":"done"}` + "\n\n"
	got := extractFinalText(strings.NewReader(stream))
	if got != "" {
		t.Fatalf("expected empty for no assistant, got %q", got)
	}
}

func TestExtractFinalText_IgnoresMalformedLines(t *testing.T) {
	stream := strings.Join([]string{
		`: keepalive`,
		`data: {not json}`,
		`data: {"event_type":"assistant","payload":{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}}`,
		`data: {"event_type":"done"}`,
		``,
	}, "\n\n")
	got := extractFinalText(strings.NewReader(stream))
	if got != "ok" {
		t.Fatalf("expected 'ok', got %q", got)
	}
}

func TestExtractFinalText_LargeSingleEvent(t *testing.T) {
	// bufio.Scanner's default 64 KiB cap would truncate this; we use ReadString.
	big := strings.Repeat("x", 256*1024)
	stream := `data: {"event_type":"assistant","payload":{"type":"assistant","message":{"content":[{"type":"text","text":"` + big + `"}]}}}` + "\n\n" +
		`data: {"event_type":"done"}` + "\n\n"
	got := extractFinalText(strings.NewReader(stream))
	if got != big {
		t.Fatalf("expected %d-byte payload, got %d bytes", len(big), len(got))
	}
}

func TestExtractFinalText_StringContent(t *testing.T) {
	// Some SDK variants emit content as a plain string. Handle it gracefully.
	stream := `data: {"event_type":"assistant","payload":{"type":"assistant","message":{"content":"plain"}}}` + "\n\n" +
		`data: {"event_type":"done"}` + "\n\n"
	got := extractFinalText(strings.NewReader(stream))
	if got != "plain" {
		t.Fatalf("expected 'plain', got %q", got)
	}
}
