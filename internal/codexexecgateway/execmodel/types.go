// Package execmodel holds shared DTOs that cross the
// codexexecgateway↔handlers package boundary. Both packages import it
// to avoid an import cycle and to eliminate field-by-field adapter
// translation that silently drops new fields.
package execmodel

import "time"

// Executor is the persistent identity of a codex-exec node.
//
// IsOnline reflects in-memory ConnRegistry membership at the moment the row
// was assembled — it is NOT a column. ClientIP/UA/CodexVersion/OS and the
// connected_at/disconnected_at pair are overwritten on each new inbound
// connect; LastSeenAt is kept for back-compat callers but is no longer
// touched on disconnect (use DisconnectedAt instead).
type Executor struct {
	ExeID          string     `json:"exe_id"`
	UserID         string     `json:"user_id"`
	DisplayName    string     `json:"display_name,omitempty"`
	Description    string     `json:"description,omitempty"`
	DefaultCwd     string     `json:"default_cwd,omitempty"`
	RegisteredAt   time.Time  `json:"registered_at"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	ClientIP       string     `json:"client_ip,omitempty"`
	ClientUA       string     `json:"client_ua,omitempty"`
	CodexVersion   string     `json:"codex_version,omitempty"`
	OS             string     `json:"os,omitempty"`
	ConnectedAt    *time.Time `json:"connected_at,omitempty"`
	DisconnectedAt *time.Time `json:"disconnected_at,omitempty"`
	IsOnline       bool       `json:"is_online"`
}

// WorkspaceExecutor is a row in workspace_executors. Name is the
// workspace-unique human-readable label LLM-facing tools surface
// (per v0.54.0); Description is the per-binding free-text note the
// user can attach when registering.
type WorkspaceExecutor struct {
	WorkspaceID string    `json:"workspace_id"`
	ExeID       string    `json:"exe_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	IsDefault   bool      `json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
}

// ConnectedExecutor is the join shape returned by workspace listing
// and /api/exec-gateway/connected endpoints. ExeID is still present
// (env-mcp uses it to dial /bridge and our internal routing keys by
// it) but LLM-facing payloads omit it.
//
// The client-info fields mirror Executor and let the workspace UI render
// IP / OS / codex version without a second roundtrip. IsOnline is set by
// the SDK adapter from the live ConnRegistry, not the DB.
type ConnectedExecutor struct {
	ExeID          string     `json:"exe_id"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	IsDefault      bool       `json:"is_default"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	ClientIP       string     `json:"client_ip,omitempty"`
	ClientUA       string     `json:"client_ua,omitempty"`
	CodexVersion   string     `json:"codex_version,omitempty"`
	OS             string     `json:"os,omitempty"`
	ConnectedAt    *time.Time `json:"connected_at,omitempty"`
	DisconnectedAt *time.Time `json:"disconnected_at,omitempty"`
	IsOnline       bool       `json:"is_online"`
}
