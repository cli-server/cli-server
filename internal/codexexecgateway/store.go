package codexexecgateway

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log"
	"sort"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store provides Postgres access for executors + workspace bindings.
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
	if _, err := s.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		var exists bool
		if err := s.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", name).Scan(&exists); err != nil {
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
			return fmt.Errorf("begin tx for migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations(version) VALUES ($1)", name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
		log.Printf("applied migration: %s", name)
	}
	return nil
}

// CreateExecutor inserts a new executor row. Caller supplies the bcrypt hash.
func (s *Store) CreateExecutor(ctx context.Context, e Executor, registrationTokenHash string) error {
	_, err := s.ExecContext(ctx, `
		INSERT INTO executors (exe_id, user_id, display_name, description, default_cwd,
		                       registration_token_hash, registered_at)
		VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), $6, $7)`,
		e.ExeID, e.UserID, e.DisplayName, e.Description, e.DefaultCwd,
		registrationTokenHash, e.RegisteredAt)
	if err != nil {
		return fmt.Errorf("insert executor: %w", err)
	}
	return nil
}

// GetExecutor returns the executor by id, or (nil, nil) if absent.
func (s *Store) GetExecutor(ctx context.Context, exeID string) (*Executor, error) {
	row := s.QueryRowContext(ctx, `
		SELECT exe_id, user_id,
		       COALESCE(display_name, ''),
		       COALESCE(description, ''),
		       COALESCE(default_cwd, ''),
		       registered_at, last_seen_at
		FROM executors WHERE exe_id=$1`, exeID)
	var e Executor
	var lastSeen sql.NullTime
	err := row.Scan(&e.ExeID, &e.UserID, &e.DisplayName, &e.Description, &e.DefaultCwd,
		&e.RegisteredAt, &lastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get executor: %w", err)
	}
	if lastSeen.Valid {
		t := lastSeen.Time
		e.LastSeenAt = &t
	}
	return &e, nil
}

// GetRegistrationTokenHash returns the bcrypt hash used to authenticate
// /codex-exec/{exe_id} ws connections.
func (s *Store) GetRegistrationTokenHash(ctx context.Context, exeID string) (string, error) {
	var hash string
	err := s.QueryRowContext(ctx, `SELECT registration_token_hash FROM executors WHERE exe_id=$1`, exeID).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get registration token hash: %w", err)
	}
	return hash, nil
}

// UpdateLastSeen sets the last_seen_at timestamp to NOW().
func (s *Store) UpdateLastSeen(ctx context.Context, exeID string) error {
	_, err := s.ExecContext(ctx, `UPDATE executors SET last_seen_at=NOW() WHERE exe_id=$1`, exeID)
	if err != nil {
		return fmt.Errorf("update last_seen: %w", err)
	}
	return nil
}

// Close closes the underlying DB.
func (s *Store) Close() error { return s.DB.Close() }

// BindWorkspaceExecutor inserts a workspace ↔ executor binding (or upserts is_default).
func (s *Store) BindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string, isDefault bool) error {
	_, err := s.ExecContext(ctx, `
		INSERT INTO workspace_executors (workspace_id, exe_id, is_default)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, exe_id)
		DO UPDATE SET is_default = EXCLUDED.is_default`,
		workspaceID, exeID, isDefault)
	if err != nil {
		return fmt.Errorf("bind workspace executor: %w", err)
	}
	return nil
}

// UnbindWorkspaceExecutor removes a binding row.
func (s *Store) UnbindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string) error {
	_, err := s.ExecContext(ctx, `
		DELETE FROM workspace_executors
		WHERE workspace_id=$1 AND exe_id=$2`, workspaceID, exeID)
	if err != nil {
		return fmt.Errorf("unbind workspace executor: %w", err)
	}
	return nil
}

// ListWorkspaceExecutors returns all bindings for a workspace, joined with executor metadata.
func (s *Store) ListWorkspaceExecutors(ctx context.Context, workspaceID string) ([]ConnectedExecutor, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT we.exe_id,
		       COALESCE(e.description, ''),
		       COALESCE(e.default_cwd, ''),
		       we.is_default,
		       e.last_seen_at
		FROM workspace_executors we
		JOIN executors e ON e.exe_id = we.exe_id
		WHERE we.workspace_id = $1
		ORDER BY we.created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace executors: %w", err)
	}
	defer rows.Close()
	var out []ConnectedExecutor
	for rows.Next() {
		var c ConnectedExecutor
		var lastSeen sql.NullTime
		if err := rows.Scan(&c.ExeID, &c.Description, &c.DefaultCwd, &c.IsDefault, &lastSeen); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			t := lastSeen.Time
			c.LastSeenAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConnectedExecutorsForWorkspace returns the intersection of (workspace's bound
// executors) ∩ (the connected exe_id list passed in). Used by the internal
// `/api/exec-gateway/connected` endpoint.
func (s *Store) ConnectedExecutorsForWorkspace(ctx context.Context, workspaceID string, connectedIDs []string) ([]ConnectedExecutor, error) {
	all, err := s.ListWorkspaceExecutors(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	connSet := make(map[string]struct{}, len(connectedIDs))
	for _, id := range connectedIDs {
		connSet[id] = struct{}{}
	}
	var out []ConnectedExecutor
	for _, c := range all {
		if _, ok := connSet[c.ExeID]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}
