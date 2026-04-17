package server

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// extractFinalText consumes the cc-broker SSE stream (one event per
// `data: ...\n\n`) and returns the final assistant text for the turn.
//
// The cc-broker streams bridge events verbatim. Each event has the shape:
//
//	{
//	  "event_type": "assistant" | "user" | "result" | "system" | "done",
//	  "payload": { "type": "assistant", "message": { "content": [...] }, ... },
//	  ...
//	}
//
// For assistant messages the content is an array of blocks of the form
// `{"type": "text", "text": "..."}` or `{"type": "tool_use", ...}`; we only
// keep the text blocks. For the terminal `result` event the final answer is
// under the top-level `result` key (CC's synthesized reply string) and is
// preferred when present.
//
// Returns an empty string if the stream contains no assistant/result text.
func extractFinalText(body io.Reader) string {
	// bufio.Scanner's default 64 KiB line limit is too small — a single
	// assistant message with tool output can easily exceed it.
	reader := bufio.NewReaderSize(body, 1<<20)

	var lastAssistantText string
	var resultText string

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				if t, stop := parseSSEEvent(data); stop {
					break
				} else if t.resultText != "" {
					resultText = t.resultText
				} else if t.assistantText != "" {
					lastAssistantText = t.assistantText
				}
			}
		}
		if err != nil {
			break
		}
	}

	if resultText != "" {
		return resultText
	}
	return lastAssistantText
}

type sseEventText struct {
	assistantText string
	resultText    string
}

// parseSSEEvent extracts text from a single SSE `data:` payload.
// Returns stop=true when the terminating `done` event is seen.
func parseSSEEvent(data string) (text sseEventText, stop bool) {
	var env struct {
		EventType string          `json:"event_type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return sseEventText{}, false
	}
	if env.EventType == "done" {
		return sseEventText{}, true
	}
	if len(env.Payload) == 0 {
		return sseEventText{}, false
	}

	// CC SDK payload envelope: `{"type": "assistant"|"user"|"result"|..., ...}`
	var payloadHead struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Result  string          `json:"result"`
	}
	if err := json.Unmarshal(env.Payload, &payloadHead); err != nil {
		return sseEventText{}, false
	}

	switch payloadHead.Type {
	case "assistant":
		return sseEventText{assistantText: extractAssistantText(payloadHead.Message)}, false
	case "result":
		// CC's synthesized final result — preferred when non-empty.
		return sseEventText{resultText: payloadHead.Result}, false
	}
	return sseEventText{}, false
}

// extractAssistantText pulls the concatenated text from the content blocks
// of a CC SDK assistant `message`. Tool-use and other non-text blocks are
// ignored.
func extractAssistantText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	if len(msg.Content) == 0 {
		return ""
	}

	// content may be either a string (rare) or an array of blocks.
	if len(msg.Content) > 0 && msg.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(msg.Content, &s); err == nil {
			return s
		}
		return ""
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return ""
	}

	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
