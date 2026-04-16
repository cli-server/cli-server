package ccbroker

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store provides database access for the cc-broker.
type Store struct {
	*sql.DB
}

// NewStore opens a database connection and runs migrations.
func NewStore(databaseURL string) (*Store, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		name := entry.Name()
		var exists bool
		if err := s.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)", name).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists {
			continue
		}

		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := s.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", name, err)
		}
		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("execute migration %s: %w", name, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
		log.Printf("Applied migration: %s", name)
	}

	return nil
}

// CreateSession inserts a new session.
func (s *Store) CreateSession(ctx context.Context, id, workspaceID, title, source string, externalID *string) error {
	_, err := s.ExecContext(ctx,
		`INSERT INTO agent_sessions (id, workspace_id, title, source, external_id, status, epoch)
		 VALUES ($1, $2, $3, $4, $5, 'active', 0)`,
		id, workspaceID, title, source, externalID,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession retrieves a session by ID. Returns nil if not found.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	sess := &Session{}
	var externalID sql.NullString
	err := s.QueryRowContext(ctx,
		`SELECT id, workspace_id, title, status, epoch, external_id, source, created_at
		 FROM agent_sessions WHERE id = $1`, id,
	).Scan(&sess.ID, &sess.WorkspaceID, &sess.Title, &sess.Status, &sess.Epoch, &externalID, &sess.Source, &sess.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if externalID.Valid {
		sess.ExternalID = &externalID.String
	}
	return sess, nil
}

// BumpSessionEpoch atomically increments the epoch and returns the new value.
func (s *Store) BumpSessionEpoch(ctx context.Context, id string) (int, error) {
	var epoch int
	err := s.QueryRowContext(ctx,
		`UPDATE agent_sessions SET epoch = epoch + 1, updated_at = NOW()
		 WHERE id = $1 RETURNING epoch`, id,
	).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("session not found: %s", id)
	}
	if err != nil {
		return 0, fmt.Errorf("bump session epoch: %w", err)
	}
	return epoch, nil
}

// GetSessionEpoch returns the current epoch for a session.
func (s *Store) GetSessionEpoch(ctx context.Context, sessionID string) (int, error) {
	var epoch int
	err := s.QueryRowContext(ctx,
		`SELECT epoch FROM agent_sessions WHERE id = $1`, sessionID,
	).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("session not found: %s", sessionID)
	}
	if err != nil {
		return 0, fmt.Errorf("get session epoch: %w", err)
	}
	return epoch, nil
}

// InsertEvents inserts a batch of events, skipping duplicates.
// Returns only the successfully inserted events with their sequence numbers.
func (s *Store) InsertEvents(ctx context.Context, sessionID string, epoch int, events []EventInput) ([]InsertedEvent, error) {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO agent_session_events (session_id, event_id, event_type, source, epoch, payload, ephemeral)
		 VALUES ($1, $2, 'client_event', 'worker', $3, $4, $5)
		 ON CONFLICT (event_id) DO NOTHING
		 RETURNING id`,
	)
	if err != nil {
		return nil, fmt.Errorf("prepare insert events: %w", err)
	}
	defer stmt.Close()

	var inserted []InsertedEvent
	for _, e := range events {
		var seqNum int64
		err := stmt.QueryRowContext(ctx, sessionID, e.EventID, epoch, e.Payload, e.Ephemeral).Scan(&seqNum)
		if errors.Is(err, sql.ErrNoRows) {
			continue // duplicate event_id — skip
		}
		if err != nil {
			return nil, fmt.Errorf("insert event %s: %w", e.EventID, err)
		}
		inserted = append(inserted, InsertedEvent{SeqNum: seqNum, EventID: e.EventID})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit insert events: %w", err)
	}
	return inserted, nil
}

// GetEventsSince returns events with sequence number > sinceSeqNum.
func (s *Store) GetEventsSince(ctx context.Context, sessionID string, sinceSeqNum int64, limit int) ([]SessionEvent, error) {
	rows, err := s.QueryContext(ctx,
		`SELECT id, session_id, event_id, event_type, source, epoch, payload, ephemeral, created_at
		 FROM agent_session_events
		 WHERE session_id = $1 AND id > $2
		 ORDER BY id ASC
		 LIMIT $3`,
		sessionID, sinceSeqNum, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get events since: %w", err)
	}
	defer rows.Close()

	var events []SessionEvent
	for rows.Next() {
		var e SessionEvent
		if err := rows.Scan(&e.ID, &e.SessionID, &e.EventID, &e.EventType, &e.Source, &e.Epoch, &e.Payload, &e.Ephemeral, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// InsertInternalEvents inserts a batch of internal events.
func (s *Store) InsertInternalEvents(ctx context.Context, sessionID string, events []InternalEventInput) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO agent_session_internal_events (session_id, event_type, payload, is_compaction, agent_id)
		 VALUES ($1, $2, $3, $4, $5)`,
	)
	if err != nil {
		return fmt.Errorf("prepare insert internal events: %w", err)
	}
	defer stmt.Close()

	for _, e := range events {
		agentID := sql.NullString{String: e.AgentID, Valid: e.AgentID != ""}
		if _, err := stmt.ExecContext(ctx, sessionID, e.EventType, e.Payload, e.IsCompaction, agentID); err != nil {
			return fmt.Errorf("insert internal event: %w", err)
		}
	}

	return tx.Commit()
}

// GetInternalEventsSince returns internal events with id > sinceID.
func (s *Store) GetInternalEventsSince(ctx context.Context, sessionID string, sinceID int64, limit int) ([]SessionEvent, error) {
	rows, err := s.QueryContext(ctx,
		`SELECT id, event_type, payload, is_compaction, COALESCE(agent_id, ''), created_at
		 FROM agent_session_internal_events
		 WHERE session_id = $1 AND id > $2
		 ORDER BY id ASC LIMIT $3`,
		sessionID, sinceID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get internal events since: %w", err)
	}
	defer rows.Close()

	var events []SessionEvent
	for rows.Next() {
		var e SessionEvent
		var isCompaction bool
		var agentID string
		if err := rows.Scan(&e.ID, &e.EventType, &e.Payload, &isCompaction, &agentID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan internal event: %w", err)
		}
		e.SessionID = sessionID
		e.Source = "internal"
		events = append(events, e)
	}
	return events, rows.Err()
}

// UpsertWorker inserts or updates a worker registration.
func (s *Store) UpsertWorker(ctx context.Context, sessionID string, epoch int) error {
	_, err := s.ExecContext(ctx,
		`INSERT INTO agent_session_workers (session_id, epoch)
		 VALUES ($1, $2)
		 ON CONFLICT (session_id, epoch) DO UPDATE SET registered_at = NOW()`,
		sessionID, epoch,
	)
	if err != nil {
		return fmt.Errorf("upsert worker: %w", err)
	}
	return nil
}

// UpdateWorkerState updates the state and metadata for a worker.
func (s *Store) UpdateWorkerState(ctx context.Context, sessionID string, epoch int, state string, metadata, actionDetails json.RawMessage) error {
	_, err := s.ExecContext(ctx,
		`UPDATE agent_session_workers
		 SET state = $3, external_metadata = $4, requires_action_details = $5, last_heartbeat_at = NOW()
		 WHERE session_id = $1 AND epoch = $2`,
		sessionID, epoch, state, metadata, actionDetails,
	)
	if err != nil {
		return fmt.Errorf("update worker state: %w", err)
	}
	return nil
}

// UpdateWorkerHeartbeat updates the heartbeat timestamp for a worker.
func (s *Store) UpdateWorkerHeartbeat(ctx context.Context, sessionID string, epoch int) error {
	_, err := s.ExecContext(ctx,
		`UPDATE agent_session_workers SET last_heartbeat_at = NOW()
		 WHERE session_id = $1 AND epoch = $2`,
		sessionID, epoch,
	)
	if err != nil {
		return fmt.Errorf("update worker heartbeat: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.DB.Close()
}
