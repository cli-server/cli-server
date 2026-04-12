package server

import (
	"encoding/json"
	"strings"

	"github.com/agentserver/agentserver/internal/db"
)

// extractTaskOutput converts session events into human-readable text.
// It extracts assistant text blocks and tool_result content from claude CLI messages.
func extractTaskOutput(events []db.AgentSessionEvent) string {
	var parts []string

	for _, e := range events {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(e.Payload, &msg); err != nil {
			continue
		}

		for _, block := range msg.Message.Content {
			var cb struct {
				Type    string          `json:"type"`
				Text    string          `json:"text"`
				Content json.RawMessage `json:"content"` // string or []contentBlock
			}
			if err := json.Unmarshal(block, &cb); err != nil {
				continue
			}

			switch cb.Type {
			case "text":
				if cb.Text != "" {
					parts = append(parts, cb.Text)
				}
			case "tool_result":
				parts = append(parts, extractToolResultContent(cb.Content)...)
			}
		}
	}

	return strings.Join(parts, "\n")
}

// extractToolResultContent handles tool_result content which can be either
// a plain string or an array of content blocks [{type, text}, ...].
func extractToolResultContent(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []string{s}
	}
	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return parts
	}
	return nil
}
