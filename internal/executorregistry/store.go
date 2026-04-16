package executorregistry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store provides database access for the executor registry.
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

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CreateExecutor inserts an executor and its associated capabilities and heartbeat rows.
func (s *Store) CreateExecutor(ctx context.Context, executor Executor, tunnelToken, registryToken string) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var tunnelHash, registryHash *string
	if tunnelToken != "" {
		h := hashToken(tunnelToken)
		tunnelHash = &h
	}
	if registryToken != "" {
		h := hashToken(registryToken)
		registryHash = &h
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO executors (id, workspace_id, name, type, status, tunnel_token_hash, registry_token_hash, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		executor.ID, executor.WorkspaceID, executor.Name, executor.Type, executor.Status,
		tunnelHash, registryHash, executor.CreatedAt, executor.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert executor: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO executor_capabilities (executor_id) VALUES ($1)`,
		executor.ID,
	)
	if err != nil {
		return fmt.Errorf("insert capabilities: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO executor_heartbeats (executor_id) VALUES ($1)`,
		executor.ID,
	)
	if err != nil {
		return fmt.Errorf("insert heartbeat: %w", err)
	}

	return tx.Commit()
}

// GetExecutor retrieves a single executor by ID. Returns nil if not found.
func (s *Store) GetExecutor(ctx context.Context, id string) (*ExecutorInfo, error) {
	row := s.QueryRowContext(ctx, `
		SELECT e.id, e.workspace_id, e.name, e.type, e.status, e.created_at, e.updated_at,
		       c.tools, c.environment, c.resources, c.description, c.working_dir, c.probed_at,
		       h.last_seen
		FROM executors e
		LEFT JOIN executor_capabilities c ON c.executor_id = e.id
		LEFT JOIN executor_heartbeats h ON h.executor_id = e.id
		WHERE e.id = $1`, id)

	info, err := scanExecutorInfo(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get executor %s: %w", id, err)
	}
	return info, nil
}

// ListExecutors returns all executors in a workspace with their capabilities and last heartbeat.
func (s *Store) ListExecutors(ctx context.Context, workspaceID string) ([]ExecutorInfo, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT e.id, e.workspace_id, e.name, e.type, e.status, e.created_at, e.updated_at,
		       c.tools, c.environment, c.resources, c.description, c.working_dir, c.probed_at,
		       h.last_seen
		FROM executors e
		LEFT JOIN executor_capabilities c ON c.executor_id = e.id
		LEFT JOIN executor_heartbeats h ON h.executor_id = e.id
		WHERE e.workspace_id = $1
		ORDER BY e.created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list executors: %w", err)
	}
	defer rows.Close()

	var result []ExecutorInfo
	for rows.Next() {
		info, err := scanExecutorInfoRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan executor: %w", err)
		}
		result = append(result, *info)
	}
	return result, rows.Err()
}

// ValidateTunnelToken checks whether the given tunnel token matches the stored hash.
func (s *Store) ValidateTunnelToken(ctx context.Context, executorID, token string) (bool, error) {
	var storedHash sql.NullString
	err := s.QueryRowContext(ctx, `SELECT tunnel_token_hash FROM executors WHERE id = $1`, executorID).Scan(&storedHash)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate tunnel token: %w", err)
	}
	if !storedHash.Valid {
		return false, nil
	}
	return storedHash.String == hashToken(token), nil
}

// ValidateRegistryToken checks whether the given registry token matches the stored hash.
func (s *Store) ValidateRegistryToken(ctx context.Context, executorID, token string) (bool, error) {
	var storedHash sql.NullString
	err := s.QueryRowContext(ctx, `SELECT registry_token_hash FROM executors WHERE id = $1`, executorID).Scan(&storedHash)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate registry token: %w", err)
	}
	if !storedHash.Valid {
		return false, nil
	}
	return storedHash.String == hashToken(token), nil
}

// UpdateHeartbeat updates the last_seen timestamp and system_info for an executor.
func (s *Store) UpdateHeartbeat(ctx context.Context, executorID string, systemInfo json.RawMessage) error {
	if systemInfo == nil {
		systemInfo = json.RawMessage("{}")
	}
	_, err := s.ExecContext(ctx, `
		UPDATE executor_heartbeats SET last_seen = NOW(), system_info = $2
		WHERE executor_id = $1`, executorID, systemInfo)
	if err != nil {
		return fmt.Errorf("update heartbeat: %w", err)
	}
	return nil
}

// UpdateCapabilities updates the capability fields for an executor.
func (s *Store) UpdateCapabilities(ctx context.Context, executorID string, cap ExecutorCapability) error {
	toolsJSON, err := json.Marshal(cap.Tools)
	if err != nil {
		return fmt.Errorf("marshal tools: %w", err)
	}
	envJSON, err := json.Marshal(cap.Environment)
	if err != nil {
		return fmt.Errorf("marshal environment: %w", err)
	}
	resJSON, err := json.Marshal(cap.Resources)
	if err != nil {
		return fmt.Errorf("marshal resources: %w", err)
	}

	_, err = s.ExecContext(ctx, `
		UPDATE executor_capabilities
		SET tools = $2, environment = $3, resources = $4, description = $5, working_dir = $6, probed_at = NOW()
		WHERE executor_id = $1`,
		executorID, toolsJSON, envJSON, resJSON, cap.Description, cap.WorkingDir)
	if err != nil {
		return fmt.Errorf("update capabilities: %w", err)
	}
	return nil
}

// UpdateExecutorStatus updates the status of an executor.
func (s *Store) UpdateExecutorStatus(ctx context.Context, executorID, status string) error {
	_, err := s.ExecContext(ctx, `
		UPDATE executors SET status = $2, updated_at = NOW()
		WHERE id = $1`, executorID, status)
	if err != nil {
		return fmt.Errorf("update executor status: %w", err)
	}
	return nil
}

// scannable is an interface satisfied by both *sql.Row and *sql.Rows.
type scannable interface {
	Scan(dest ...any) error
}

func scanExecutorInfo(row *sql.Row) (*ExecutorInfo, error) {
	return scanFromScannable(row)
}

func scanExecutorInfoRow(row *sql.Rows) (*ExecutorInfo, error) {
	return scanFromScannable(row)
}

func scanFromScannable(s scannable) (*ExecutorInfo, error) {
	var info ExecutorInfo
	var toolsJSON, envJSON, resJSON []byte
	var description, workingDir sql.NullString
	var probedAt sql.NullTime
	var lastSeen sql.NullTime

	err := s.Scan(
		&info.ID, &info.WorkspaceID, &info.Name, &info.Type, &info.Status,
		&info.CreatedAt, &info.UpdatedAt,
		&toolsJSON, &envJSON, &resJSON, &description, &workingDir, &probedAt,
		&lastSeen,
	)
	if err != nil {
		return nil, err
	}

	// Unmarshal JSONB fields
	if toolsJSON != nil {
		if err := json.Unmarshal(toolsJSON, &info.Capabilities.Tools); err != nil {
			return nil, fmt.Errorf("unmarshal tools: %w", err)
		}
	}
	if envJSON != nil {
		if err := json.Unmarshal(envJSON, &info.Capabilities.Environment); err != nil {
			return nil, fmt.Errorf("unmarshal environment: %w", err)
		}
	}
	if resJSON != nil {
		if err := json.Unmarshal(resJSON, &info.Capabilities.Resources); err != nil {
			return nil, fmt.Errorf("unmarshal resources: %w", err)
		}
	}

	info.Capabilities.ExecutorID = info.ID
	if description.Valid {
		info.Capabilities.Description = description.String
	}
	if workingDir.Valid {
		info.Capabilities.WorkingDir = workingDir.String
	}
	if probedAt.Valid {
		t := probedAt.Time
		info.Capabilities.ProbedAt = &t
	}
	if lastSeen.Valid {
		t := lastSeen.Time
		info.LastSeen = &t
	}

	return &info, nil
}

// Close closes the underlying database connection. Satisfies io.Closer.
func (s *Store) Close() error {
	return s.DB.Close()
}

// Ensure Store implements the Closer interface at compile time.
var _ interface{ Close() error } = (*Store)(nil)
