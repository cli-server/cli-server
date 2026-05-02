package ccbroker

import (
	"context"
	"database/sql"
	"embed"
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

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.DB.Close()
}
