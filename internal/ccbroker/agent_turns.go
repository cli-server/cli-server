package ccbroker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AgentTurn mirrors a row in the agent_turns table.
type AgentTurn struct {
	ID          string
	SessionID   string
	WorkspaceID string
	State       string // queued|running|done|cancelled|failed
	UserEventID string
	UserMessage string
	Metadata    json.RawMessage
	IMChannelID sql.NullString
	IMUserID    sql.NullString
	ErrorMsg    sql.NullString
	EnqueuedAt  time.Time
	StartedAt   sql.NullTime
	FinishedAt  sql.NullTime
}

func (s *Store) EnqueueTurn(ctx context.Context, t AgentTurn) error {
	meta := t.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	_, err := s.ExecContext(ctx,
		`INSERT INTO agent_turns
		   (id, session_id, workspace_id, state, user_event_id, user_message,
		    metadata, im_channel_id, im_user_id)
		 VALUES ($1, $2, $3, 'queued', $4, $5, $6, $7, $8)`,
		t.ID, t.SessionID, t.WorkspaceID, t.UserEventID, t.UserMessage,
		meta, nullableString(t.IMChannelID), nullableString(t.IMUserID),
	)
	if err != nil {
		return fmt.Errorf("enqueue turn: %w", err)
	}
	return nil
}

func (s *Store) PickNextPending(ctx context.Context, sessionID string) (*AgentTurn, error) {
	row := s.QueryRowContext(ctx,
		`SELECT id, session_id, workspace_id, state, user_event_id, user_message,
		        metadata, im_channel_id, im_user_id, error_msg,
		        enqueued_at, started_at, finished_at
		 FROM agent_turns
		 WHERE session_id = $1 AND state IN ('queued','running')
		 ORDER BY enqueued_at ASC
		 LIMIT 1`, sessionID)
	t := &AgentTurn{}
	err := row.Scan(&t.ID, &t.SessionID, &t.WorkspaceID, &t.State, &t.UserEventID, &t.UserMessage,
		&t.Metadata, &t.IMChannelID, &t.IMUserID, &t.ErrorMsg,
		&t.EnqueuedAt, &t.StartedAt, &t.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick next pending: %w", err)
	}
	return t, nil
}

func (s *Store) MarkTurnRunning(ctx context.Context, turnID string) error {
	res, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='running', started_at=NOW()
		 WHERE id=$1 AND state='queued'`, turnID)
	if err != nil {
		return fmt.Errorf("mark turn running: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either already-running (recovery path) or terminal — caller decides.
		return nil
	}
	return nil
}

func (s *Store) MarkTurnDone(ctx context.Context, turnID string) error {
	_, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='done', finished_at=NOW()
		 WHERE id=$1 AND state IN ('running','queued')`, turnID)
	if err != nil {
		return fmt.Errorf("mark turn done: %w", err)
	}
	return nil
}

func (s *Store) MarkTurnCancelled(ctx context.Context, turnID string) error {
	_, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='cancelled', finished_at=NOW()
		 WHERE id=$1 AND state IN ('queued','running')`, turnID)
	if err != nil {
		return fmt.Errorf("mark turn cancelled: %w", err)
	}
	return nil
}

func (s *Store) MarkTurnFailed(ctx context.Context, turnID, errMsg string) error {
	_, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='failed', finished_at=NOW(), error_msg=$2
		 WHERE id=$1 AND state IN ('queued','running')`, turnID, errMsg)
	if err != nil {
		return fmt.Errorf("mark turn failed: %w", err)
	}
	return nil
}

func (s *Store) GetTurn(ctx context.Context, turnID string) (*AgentTurn, error) {
	row := s.QueryRowContext(ctx,
		`SELECT id, session_id, workspace_id, state, user_event_id, user_message,
		        metadata, im_channel_id, im_user_id, error_msg,
		        enqueued_at, started_at, finished_at
		 FROM agent_turns WHERE id=$1`, turnID)
	t := &AgentTurn{}
	err := row.Scan(&t.ID, &t.SessionID, &t.WorkspaceID, &t.State, &t.UserEventID, &t.UserMessage,
		&t.Metadata, &t.IMChannelID, &t.IMUserID, &t.ErrorMsg,
		&t.EnqueuedAt, &t.StartedAt, &t.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get turn: %w", err)
	}
	return t, nil
}

func (s *Store) ListSessionsWithPending(ctx context.Context) ([]string, error) {
	rows, err := s.QueryContext(ctx,
		`SELECT DISTINCT session_id FROM agent_turns WHERE state IN ('queued','running')`)
	if err != nil {
		return nil, fmt.Errorf("list pending sessions: %w", err)
	}
	defer rows.Close()
	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("scan sid: %w", err)
		}
		sids = append(sids, sid)
	}
	return sids, rows.Err()
}

func (s *Store) ListSessionTurns(ctx context.Context, sessionID string, limit int) ([]AgentTurn, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.QueryContext(ctx,
		`SELECT id, session_id, workspace_id, state, user_event_id, user_message,
		        metadata, im_channel_id, im_user_id, error_msg,
		        enqueued_at, started_at, finished_at
		 FROM agent_turns WHERE session_id=$1
		 ORDER BY enqueued_at DESC LIMIT $2`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list session turns: %w", err)
	}
	defer rows.Close()
	var out []AgentTurn
	for rows.Next() {
		var t AgentTurn
		if err := rows.Scan(&t.ID, &t.SessionID, &t.WorkspaceID, &t.State, &t.UserEventID, &t.UserMessage,
			&t.Metadata, &t.IMChannelID, &t.IMUserID, &t.ErrorMsg,
			&t.EnqueuedAt, &t.StartedAt, &t.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ResetRunningToQueued(ctx context.Context) (int, error) {
	res, err := s.ExecContext(ctx,
		`UPDATE agent_turns SET state='queued', started_at=NULL
		 WHERE state='running'`)
	if err != nil {
		return 0, fmt.Errorf("reset running to queued: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) CountPending(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_turns
		 WHERE session_id=$1 AND state IN ('queued','running')`,
		sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// InsertEventsWithTurn is identical to InsertEvents but tags each row with turn_id.
func (s *Store) InsertEventsWithTurn(ctx context.Context, sessionID string, epoch int, turnID string, events []EventInput) ([]InsertedEvent, error) {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO agent_session_events
		   (session_id, event_id, event_type, source, epoch, payload, ephemeral, turn_id)
		 VALUES ($1, $2, 'client_event', 'worker', $3, $4, $5, $6)
		 ON CONFLICT (event_id) DO NOTHING
		 RETURNING id`)
	if err != nil {
		return nil, fmt.Errorf("prepare insert events with turn: %w", err)
	}
	defer stmt.Close()
	var inserted []InsertedEvent
	for _, e := range events {
		var seqNum int64
		var tid interface{} = turnID
		if turnID == "" {
			tid = nil
		}
		err := stmt.QueryRowContext(ctx, sessionID, e.EventID, epoch, e.Payload, e.Ephemeral, tid).Scan(&seqNum)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("insert event %s: %w", e.EventID, err)
		}
		inserted = append(inserted, InsertedEvent{SeqNum: seqNum, EventID: e.EventID})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit insert events with turn: %w", err)
	}
	return inserted, nil
}

func nullableString(v sql.NullString) interface{} {
	if !v.Valid {
		return nil
	}
	return v.String
}
