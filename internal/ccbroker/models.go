package ccbroker

import (
	"encoding/json"
	"time"
)

type Session struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Title       string    `json:"title,omitempty"`
	Status      string    `json:"status"`
	Epoch       int       `json:"epoch"`
	ExternalID  *string   `json:"external_id,omitempty"`
	Source      string    `json:"source,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type SessionEvent struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"session_id"`
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	Source    string          `json:"source"`
	Epoch     int             `json:"epoch"`
	Payload   json.RawMessage `json:"payload"`
	Ephemeral bool            `json:"ephemeral"`
	CreatedAt time.Time       `json:"created_at"`
}

type StreamClientEvent struct {
	EventID     string          `json:"event_id"`
	SequenceNum int64           `json:"sequence_num"`
	EventType   string          `json:"event_type"`
	Source      string          `json:"source"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   string          `json:"created_at"`
}

type EventInput struct {
	EventID   string
	Payload   json.RawMessage
	Ephemeral bool
}

type InsertedEvent struct {
	SeqNum  int64
	EventID string
}
