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
				Type    string `json:"type"`
				Text    string `json:"text"`
				Content string `json:"content"` // tool_result text content
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
				if cb.Content != "" {
					parts = append(parts, cb.Content)
				}
			}
		}
	}

	return strings.Join(parts, "\n")
}
