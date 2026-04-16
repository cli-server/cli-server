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

type WorkerJWTClaims struct {
	SessionID   string `json:"sid"`
	WorkspaceID string `json:"wid"`
	Epoch       int    `json:"epoch"`
	Exp         int64  `json:"exp"`
}

type BridgeResponse struct {
	WorkerJWT   string `json:"worker_jwt"`
	APIBaseURL  string `json:"api_base_url"`
	ExpiresIn   int    `json:"expires_in"`
	WorkerEpoch int    `json:"worker_epoch"`
}

type EventBatchRequest struct {
	WorkerEpoch int              `json:"worker_epoch"`
	Events      []EventBatchItem `json:"events"`
}

type EventBatchItem struct {
	Payload   json.RawMessage `json:"payload"`
	Ephemeral bool            `json:"ephemeral"`
}

type InternalEventBatchRequest struct {
	WorkerEpoch int                      `json:"worker_epoch"`
	Events      []InternalEventBatchItem `json:"events"`
}

type InternalEventBatchItem struct {
	Payload      json.RawMessage `json:"payload"`
	IsCompaction bool            `json:"is_compaction"`
	AgentID      string          `json:"agent_id,omitempty"`
}

type WorkerStateRequest struct {
	WorkerStatus          string          `json:"worker_status"`
	WorkerEpoch           int             `json:"worker_epoch"`
	ExternalMetadata      json.RawMessage `json:"external_metadata,omitempty"`
	RequiresActionDetails json.RawMessage `json:"requires_action_details,omitempty"`
}

type HeartbeatRequest struct {
	WorkerEpoch int `json:"worker_epoch"`
}

type EventInput struct {
	EventID   string
	Payload   json.RawMessage
	Ephemeral bool
}

type InternalEventInput struct {
	EventType    string
	Payload      json.RawMessage
	IsCompaction bool
	AgentID      string
}

type InsertedEvent struct {
	SeqNum  int64
	EventID string
}

type Worker struct {
	SessionID             string
	Epoch                 int
	State                 string
	ExternalMetadata      json.RawMessage
	RequiresActionDetails json.RawMessage
	LastHeartbeatAt       *time.Time
	RegisteredAt          time.Time
}
