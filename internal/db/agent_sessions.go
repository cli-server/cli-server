package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// AgentSession represents a bridge session.
type AgentSession struct {
	ID          string
	SandboxID   *string
	WorkspaceID string
	Title       string
	Status      string
	Epoch       int
	Tags        []string
	IMChannelID *string // set when session is created from an IM inbound message
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ArchivedAt  sql.NullTime
}

// AgentSessionEvent is a single event in a session's event log.
type AgentSessionEvent struct {
	ID        int64           // sequence_num (BIGSERIAL)
	SessionID string
	EventID   string
	EventType string
	Source    string
	Epoch     int
	Payload   json.RawMessage
	Ephemeral bool
	CreatedAt time.Time
}

// AgentSessionWorker tracks worker state per epoch.
type AgentSessionWorker struct {
	SessionID             string
	Epoch                 int
	State                 string
	ExternalMetadata      json.RawMessage
	RequiresActionDetails json.RawMessage
	LastHeartbeatAt       time.Time
	RegisteredAt          time.Time
}

func (db *DB) CreateAgentSession(id string, sandboxID *string, workspaceID, title string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	_, err := db.Exec(
		`INSERT INTO agent_sessions (id, sandbox_id, workspace_id, title, tags)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, sandboxID, workspaceID, title, pq.Array(tags),
	)
	return err
}

func (db *DB) GetAgentSession(id string) (*AgentSession, error) {
	s := &AgentSession{}
	var tags pq.StringArray
	var sandboxID, imChannelID *string
	err := db.QueryRow(
		`SELECT id, sandbox_id, workspace_id, title, status, epoch, tags, im_channel_id, created_at, updated_at, archived_at
		 FROM agent_sessions WHERE id = $1`, id,
	).Scan(&s.ID, &sandboxID, &s.WorkspaceID, &s.Title, &s.Status, &s.Epoch, &tags, &imChannelID, &s.CreatedAt, &s.UpdatedAt, &s.ArchivedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.SandboxID = sandboxID
	s.IMChannelID = imChannelID
	s.Tags = tags
	return s, nil
}

// BumpAgentSessionEpoch atomically increments the epoch and returns the new value.
func (db *DB) BumpAgentSessionEpoch(id string) (int, error) {
	var epoch int
	err := db.QueryRow(
		`UPDATE agent_sessions SET epoch = epoch + 1, updated_at = NOW()
		 WHERE id = $1 AND status = 'active' RETURNING epoch`, id,
	).Scan(&epoch)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("session not found or not active: %s", id)
	}
	return epoch, err
}

// GetAgentSessionEpoch returns the current epoch for a session.
func (db *DB) GetAgentSessionEpoch(id string) (int, error) {
	var epoch int
	err := db.QueryRow(`SELECT epoch FROM agent_sessions WHERE id = $1`, id).Scan(&epoch)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("session not found: %s", id)
	}
	return epoch, err
}

func (db *DB) ArchiveAgentSession(id string) error {
	_, err := db.Exec(
		`UPDATE agent_sessions SET status = 'archived', archived_at = NOW(), updated_at = NOW()
		 WHERE id = $1`, id,
	)
	return err
}

// InsertedEvent pairs an event with its assigned sequence number.
type InsertedEvent struct {
	SeqNum int64
	Event  AgentSessionEvent
}

// InsertAgentSessionEvents inserts a batch of events, skipping duplicates.
// Returns only the successfully inserted events with their sequence numbers.
func (db *DB) InsertAgentSessionEvents(sessionID string, events []AgentSessionEvent) ([]InsertedEvent, error) {
	var inserted []InsertedEvent
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO agent_session_events (session_id, event_id, event_type, source, epoch, payload, ephemeral)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (event_id) DO NOTHING
		 RETURNING id`,
	)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	for _, e := range events {
		var seqNum int64
		err := stmt.QueryRow(sessionID, e.EventID, e.EventType, e.Source, e.Epoch, e.Payload, e.Ephemeral).Scan(&seqNum)
		if err == sql.ErrNoRows {
			continue // duplicate event_id — skip
		}
		if err != nil {
			return nil, err
		}
		inserted = append(inserted, InsertedEvent{SeqNum: seqNum, Event: e})
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return inserted, nil
}

// GetAgentSessionEventsSince returns events with sequence_num > sinceSeqNum.
func (db *DB) GetAgentSessionEventsSince(sessionID string, sinceSeqNum int64, limit int) ([]AgentSessionEvent, error) {
	rows, err := db.Query(
		`SELECT id, session_id, event_id, event_type, source, epoch, payload, ephemeral, created_at
		 FROM agent_session_events
		 WHERE session_id = $1 AND id > $2
		 ORDER BY id ASC
		 LIMIT $3`,
		sessionID, sinceSeqNum, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []AgentSessionEvent
	for rows.Next() {
		var e AgentSessionEvent
		if err := rows.Scan(&e.ID, &e.SessionID, &e.EventID, &e.EventType, &e.Source, &e.Epoch, &e.Payload, &e.Ephemeral, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (db *DB) UpsertAgentSessionWorker(sessionID string, epoch int) error {
	_, err := db.Exec(
		`INSERT INTO agent_session_workers (session_id, epoch)
		 VALUES ($1, $2)
		 ON CONFLICT (session_id, epoch) DO UPDATE SET
		   last_heartbeat_at = NOW()`,
		sessionID, epoch,
	)
	return err
}

func (db *DB) UpdateAgentSessionWorkerState(sessionID string, epoch int, state string, metadata, requiresActionDetails json.RawMessage) error {
	_, err := db.Exec(
		`UPDATE agent_session_workers
		 SET state = $3, external_metadata = COALESCE($4, external_metadata), requires_action_details = $5, last_heartbeat_at = NOW()
		 WHERE session_id = $1 AND epoch = $2`,
		sessionID, epoch, state, metadata, requiresActionDetails,
	)
	return err
}

func (db *DB) UpdateAgentSessionWorkerHeartbeat(sessionID string, epoch int) error {
	_, err := db.Exec(
		`UPDATE agent_session_workers SET last_heartbeat_at = NOW()
		 WHERE session_id = $1 AND epoch = $2`,
		sessionID, epoch,
	)
	return err
}

func (db *DB) GetAgentSessionWorker(sessionID string, epoch int) (*AgentSessionWorker, error) {
	w := &AgentSessionWorker{}
	err := db.QueryRow(
		`SELECT session_id, epoch, state, external_metadata, requires_action_details, last_heartbeat_at, registered_at
		 FROM agent_session_workers WHERE session_id = $1 AND epoch = $2`,
		sessionID, epoch,
	).Scan(&w.SessionID, &w.Epoch, &w.State, &w.ExternalMetadata, &w.RequiresActionDetails, &w.LastHeartbeatAt, &w.RegisteredAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

// InsertAgentSessionInternalEvents inserts internal events (transcript).
func (db *DB) InsertAgentSessionInternalEvents(sessionID string, events []struct {
	EventType   string
	Payload     json.RawMessage
	IsCompaction bool
	AgentID     string
}) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO agent_session_internal_events (session_id, event_type, payload, is_compaction, agent_id)
		 VALUES ($1, $2, $3, $4, $5)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range events {
		agentID := sql.NullString{String: e.AgentID, Valid: e.AgentID != ""}
		if _, err := stmt.Exec(sessionID, e.EventType, e.Payload, e.IsCompaction, agentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetAgentSessionInternalEventsSince returns internal events with id > sinceID.
func (db *DB) GetAgentSessionInternalEventsSince(sessionID string, sinceID int64, limit int) ([]struct {
	ID          int64
	EventType   string
	Payload     json.RawMessage
	IsCompaction bool
	AgentID     string
	CreatedAt   time.Time
}, error) {
	rows, err := db.Query(
		`SELECT id, event_type, payload, is_compaction, COALESCE(agent_id, ''), created_at
		 FROM agent_session_internal_events
		 WHERE session_id = $1 AND id > $2
		 ORDER BY id ASC LIMIT $3`,
		sessionID, sinceID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []struct {
		ID          int64
		EventType   string
		Payload     json.RawMessage
		IsCompaction bool
		AgentID     string
		CreatedAt   time.Time
	}
	for rows.Next() {
		var e struct {
			ID          int64
			EventType   string
			Payload     json.RawMessage
			IsCompaction bool
			AgentID     string
			CreatedAt   time.Time
		}
		if err := rows.Scan(&e.ID, &e.EventType, &e.Payload, &e.IsCompaction, &e.AgentID, &e.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// GetSessionByExternalID looks up a session by workspace and external ID.
func (db *DB) GetSessionByExternalID(ctx context.Context, workspaceID, externalID string) (*AgentSession, error) {
	s := &AgentSession{}
	var tags pq.StringArray
	var sandboxID, imChannelID *string
	err := db.QueryRowContext(ctx,
		`SELECT id, sandbox_id, workspace_id, title, status, epoch, tags, im_channel_id, created_at, updated_at, archived_at
		 FROM agent_sessions WHERE workspace_id = $1 AND external_id = $2`,
		workspaceID, externalID,
	).Scan(&s.ID, &sandboxID, &s.WorkspaceID, &s.Title, &s.Status, &s.Epoch, &tags, &imChannelID, &s.CreatedAt, &s.UpdatedAt, &s.ArchivedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.SandboxID = sandboxID
	s.IMChannelID = imChannelID
	s.Tags = tags
	return s, nil
}

// SetSessionExternalID sets the external_id for a session.
func (db *DB) SetSessionExternalID(ctx context.Context, sessionID, externalID string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE agent_sessions SET external_id = $1 WHERE id = $2`,
		externalID, sessionID,
	)
	return err
}

// SetSessionIMChannel sets the im_channel_id for a session.
// Called when a session is created from an inbound IM message so that
// CC's response can later be routed back to the correct channel.
func (db *DB) SetSessionIMChannel(ctx context.Context, sessionID, channelID string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE agent_sessions SET im_channel_id = $1 WHERE id = $2`,
		channelID, sessionID,
	)
	return err
}
