package db

import (
	"encoding/json"
	"fmt"
	"time"
)

type Message struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"session_id"`
	Role          string          `json:"role"`
	ContentText   string          `json:"content_text"`
	ContentRender json.RawMessage `json:"content_render"`
	StreamStatus  string          `json:"stream_status"`
	CreatedAt     time.Time       `json:"created_at"`
}

func (db *DB) ListMessages(sessionID string) ([]Message, error) {
	rows, err := db.Query(
		`SELECT id, session_id, role, content_text, content_render, stream_status, created_at
		 FROM messages WHERE session_id = $1 ORDER BY created_at ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.ContentText, &m.ContentRender, &m.StreamStatus, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	if messages == nil {
		messages = []Message{}
	}
	return messages, rows.Err()
}
