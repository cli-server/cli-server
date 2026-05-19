package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
	// TUI fields (added in migration 021)
	ChannelType         string
	CreatorUserID       *string    // NULL for legacy IM rows
	PreferredModel      *string
	PermissionMode      string
	PreferredExecutorID *string
	PermissionResponder *string
	ResponderAttachedAt *time.Time
	ActiveTurnID        *string
	CodexThreadID       *string `json:"codex_thread_id,omitempty"`
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
	var creatorUserID, preferredModel, preferredExecutorID, permissionResponder, activeTurnID, codexThreadID sql.NullString
	var responderAttachedAt sql.NullTime
	err := db.QueryRow(
		`SELECT id, sandbox_id, workspace_id, title, status, epoch, tags, im_channel_id, created_at, updated_at, archived_at,
		        channel_type, creator_user_id, preferred_model, permission_mode,
		        preferred_executor_id, permission_responder, responder_attached_at, active_turn_id,
		        codex_thread_id
		 FROM agent_sessions WHERE id = $1`, id,
	).Scan(&s.ID, &sandboxID, &s.WorkspaceID, &s.Title, &s.Status, &s.Epoch, &tags, &imChannelID, &s.CreatedAt, &s.UpdatedAt, &s.ArchivedAt,
		&s.ChannelType, &creatorUserID, &preferredModel, &s.PermissionMode,
		&preferredExecutorID, &permissionResponder, &responderAttachedAt, &activeTurnID,
		&codexThreadID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.SandboxID = sandboxID
	s.IMChannelID = imChannelID
	s.Tags = tags
	if creatorUserID.Valid {
		v := creatorUserID.String
		s.CreatorUserID = &v
	}
	if preferredModel.Valid {
		v := preferredModel.String
		s.PreferredModel = &v
	}
	if preferredExecutorID.Valid {
		v := preferredExecutorID.String
		s.PreferredExecutorID = &v
	}
	if permissionResponder.Valid {
		v := permissionResponder.String
		s.PermissionResponder = &v
	}
	if responderAttachedAt.Valid {
		v := responderAttachedAt.Time
		s.ResponderAttachedAt = &v
	}
	if activeTurnID.Valid {
		v := activeTurnID.String
		s.ActiveTurnID = &v
	}
	if codexThreadID.Valid {
		v := codexThreadID.String
		s.CodexThreadID = &v
	}
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

// GetAgentSessionEventsTail returns the most recent N events for the session,
// in chronological order. Used by the TUI SSE endpoint when ?tail=N is set.
func (db *DB) GetAgentSessionEventsTail(sessionID string, n int) ([]AgentSessionEvent, error) {
	if n <= 0 || n > 1000 {
		n = 200
	}
	rows, err := db.Query(`
		SELECT id, session_id, event_id, event_type, source, epoch, payload, ephemeral, created_at
		  FROM agent_session_events
		 WHERE session_id = $1
		 ORDER BY id DESC LIMIT $2`, sessionID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []AgentSessionEvent
	for rows.Next() {
		var e AgentSessionEvent
		if err := rows.Scan(&e.ID, &e.SessionID, &e.EventID, &e.EventType, &e.Source, &e.Epoch, &e.Payload, &e.Ephemeral, &e.CreatedAt); err != nil {
			return nil, err
		}
		rev = append(rev, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to chronological order.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
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
	var creatorUserID, preferredModel, preferredExecutorID, permissionResponder, activeTurnID, codexThreadID sql.NullString
	var responderAttachedAt sql.NullTime
	err := db.QueryRowContext(ctx,
		`SELECT id, sandbox_id, workspace_id, title, status, epoch, tags, im_channel_id, created_at, updated_at, archived_at,
		        channel_type, creator_user_id, preferred_model, permission_mode,
		        preferred_executor_id, permission_responder, responder_attached_at, active_turn_id,
		        codex_thread_id
		 FROM agent_sessions WHERE workspace_id = $1 AND external_id = $2`,
		workspaceID, externalID,
	).Scan(&s.ID, &sandboxID, &s.WorkspaceID, &s.Title, &s.Status, &s.Epoch, &tags, &imChannelID, &s.CreatedAt, &s.UpdatedAt, &s.ArchivedAt,
		&s.ChannelType, &creatorUserID, &preferredModel, &s.PermissionMode,
		&preferredExecutorID, &permissionResponder, &responderAttachedAt, &activeTurnID,
		&codexThreadID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.SandboxID = sandboxID
	s.IMChannelID = imChannelID
	s.Tags = tags
	if creatorUserID.Valid {
		v := creatorUserID.String
		s.CreatorUserID = &v
	}
	if preferredModel.Valid {
		v := preferredModel.String
		s.PreferredModel = &v
	}
	if preferredExecutorID.Valid {
		v := preferredExecutorID.String
		s.PreferredExecutorID = &v
	}
	if permissionResponder.Valid {
		v := permissionResponder.String
		s.PermissionResponder = &v
	}
	if responderAttachedAt.Valid {
		v := responderAttachedAt.Time
		s.ResponderAttachedAt = &v
	}
	if activeTurnID.Valid {
		v := activeTurnID.String
		s.ActiveTurnID = &v
	}
	if codexThreadID.Valid {
		v := codexThreadID.String
		s.CodexThreadID = &v
	}
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

// CreateTUISessionParams holds parameters for creating a TUI-originated session.
type CreateTUISessionParams struct {
	ID                  string
	WorkspaceID         string
	ExternalID          string
	Title               string
	CreatorUserID       string
	PermissionMode      string
	PreferredExecutorID string
	PreferredModel      string
}

// CreateAgentSessionTUI creates a new session with channel_type='tui'.
func (db *DB) CreateAgentSessionTUI(ctx context.Context, p CreateTUISessionParams) error {
	if p.PermissionMode == "" {
		p.PermissionMode = "ask"
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO agent_sessions
		  (id, sandbox_id, workspace_id, title, status, source, channel_type,
		   external_id, creator_user_id, permission_mode,
		   preferred_executor_id, preferred_model, tags)
		VALUES ($1, NULL, $2, $3, 'active', 'tui', 'tui',
		        $4, $5, $6,
		        NULLIF($7, ''), NULLIF($8, ''), '{}')`,
		p.ID, p.WorkspaceID, p.Title,
		p.ExternalID, p.CreatorUserID, p.PermissionMode,
		p.PreferredExecutorID, p.PreferredModel,
	)
	return err
}

// ClaimActiveTurn atomically sets active_turn_id if no turn is currently active.
// Returns (true, nil) if the claim succeeded, (false, nil) if another turn is active.
func (db *DB) ClaimActiveTurn(ctx context.Context, sessionID, turnID string) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE agent_sessions SET active_turn_id = $1, updated_at = NOW()
		 WHERE id = $2 AND active_turn_id IS NULL`, turnID, sessionID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// GetActiveTurn returns the current active_turn_id for a session, or "" if none.
// A missing session is treated the same as no active turn (no error returned).
func (db *DB) GetActiveTurn(ctx context.Context, sessionID string) (string, error) {
	var s sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT active_turn_id FROM agent_sessions WHERE id = $1`, sessionID).Scan(&s)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return s.String, nil
}

// ClearActiveTurn clears active_turn_id only if it matches expectedTurnID (CAS).
func (db *DB) ClearActiveTurn(ctx context.Context, sessionID, expectedTurnID string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE agent_sessions SET active_turn_id = NULL, updated_at = NOW()
		 WHERE id = $1 AND active_turn_id = $2`, sessionID, expectedTurnID)
	return err
}

// AttachResult holds the previous responder/preferred state before an AttachResponder call.
type AttachResult struct {
	PreviousResponder string
	PreviousPreferred string
}

// AttachResponder sets permission_responder (and optionally preferred_executor_id) for a session.
// Returns the previous values before the update. Uses a single CTE-based statement to avoid
// a SELECT-then-UPDATE race when two callers attach concurrently.
func (db *DB) AttachResponder(ctx context.Context, sessionID, executorID string, becomePreferred bool) (AttachResult, error) {
	var prev AttachResult
	var prevResp, prevPref sql.NullString

	var query string
	if becomePreferred {
		query = `
            WITH prev AS (
                SELECT permission_responder, preferred_executor_id
                  FROM agent_sessions
                 WHERE id = $2
                 FOR UPDATE
            )
            UPDATE agent_sessions s
               SET permission_responder    = $1,
                   preferred_executor_id   = $1,
                   responder_attached_at   = NOW(),
                   updated_at              = NOW()
              FROM prev
             WHERE s.id = $2
         RETURNING COALESCE(prev.permission_responder, ''), COALESCE(prev.preferred_executor_id, '')`
	} else {
		query = `
            WITH prev AS (
                SELECT permission_responder, preferred_executor_id
                  FROM agent_sessions
                 WHERE id = $2
                 FOR UPDATE
            )
            UPDATE agent_sessions s
               SET permission_responder    = $1,
                   responder_attached_at   = NOW(),
                   updated_at              = NOW()
              FROM prev
             WHERE s.id = $2
         RETURNING COALESCE(prev.permission_responder, ''), COALESCE(prev.preferred_executor_id, '')`
	}

	err := db.QueryRowContext(ctx, query, executorID, sessionID).Scan(&prevResp, &prevPref)
	if err == sql.ErrNoRows {
		return prev, nil
	}
	if err != nil {
		return prev, err
	}
	prev.PreviousResponder = prevResp.String
	prev.PreviousPreferred = prevPref.String
	return prev, nil
}

// ClearResponder clears permission_responder and responder_attached_at for a session.
func (db *DB) ClearResponder(ctx context.Context, sessionID string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE agent_sessions
		   SET permission_responder = NULL,
		       responder_attached_at = NULL,
		       updated_at = NOW()
		 WHERE id = $1`, sessionID)
	return err
}

// SessionListItem is a summary row returned by ListSessionsByChannel.
type SessionListItem struct {
	ID                  string
	ExternalID          string
	Title               string
	LastActivityAt      time.Time
	PermissionResponder *string
}

// ListSessionsByChannel returns sessions for a workspace filtered by channel type and executor.
// The external_id is expected to follow the pattern "<channelType>:<executorID>:<timestamp>".
func (db *DB) ListSessionsByChannel(ctx context.Context, workspaceID, channelType, executorID string, limit int) ([]SessionListItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	escape := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `%`, `\%`)
		s = strings.ReplaceAll(s, `_`, `\_`)
		return s
	}
	pattern := fmt.Sprintf("%s:%s:%%", escape(channelType), escape(executorID))

	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(external_id, ''), title, updated_at, permission_responder
		  FROM agent_sessions
		 WHERE workspace_id = $1
		   AND channel_type = $2
		   AND external_id LIKE $3 ESCAPE '\'
		   AND archived_at IS NULL
		 ORDER BY updated_at DESC
		 LIMIT $4`,
		workspaceID, channelType, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionListItem
	for rows.Next() {
		var it SessionListItem
		var resp sql.NullString
		if err := rows.Scan(&it.ID, &it.ExternalID, &it.Title, &it.LastActivityAt, &resp); err != nil {
			return nil, err
		}
		if resp.Valid {
			v := resp.String
			it.PermissionResponder = &v
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ListStaleResponders returns session IDs whose responder was attached before cutoff.
func (db *DB) ListStaleResponders(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM agent_sessions
		 WHERE permission_responder IS NOT NULL
		   AND responder_attached_at < $1`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		ids = append(ids, s)
	}
	return ids, rows.Err()
}

// SetSessionCodexThreadID updates (or clears, when threadID is nil) the
// codex_thread_id for a session. Used by the codex routing handler to
// persist the thread id after the first thread/start, and to clear it
// on thread-not-found / contextWindowExceeded so the next user message
// opens a fresh thread.
func (db *DB) SetSessionCodexThreadID(ctx context.Context, sessionID string, threadID *string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE agent_sessions SET codex_thread_id = $1 WHERE id = $2`,
		threadID, sessionID,
	)
	if err != nil {
		return fmt.Errorf("update codex_thread_id: %w", err)
	}
	return nil
}

// StaleActiveTurn pairs a session ID with a stuck active turn ID.
type StaleActiveTurn struct{ SessionID, TurnID string }

// ListStaleActiveTurns returns sessions with an active turn that has not been updated since cutoff.
func (db *DB) ListStaleActiveTurns(ctx context.Context, cutoff time.Time) ([]StaleActiveTurn, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, active_turn_id FROM agent_sessions
		 WHERE active_turn_id IS NOT NULL
		   AND updated_at < $1`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleActiveTurn
	for rows.Next() {
		var s StaleActiveTurn
		if err := rows.Scan(&s.SessionID, &s.TurnID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
