package db

import (
	"database/sql"
	"fmt"
	"time"
)

type Sandbox struct {
	ID              string
	WorkspaceID     string
	Name            string
	Type            string
	Status          string
	IsLocal         bool
	ShortID         sql.NullString
	SandboxName     sql.NullString
	PodIP           sql.NullString
	ProxyToken      sql.NullString
	OpencodeToken   sql.NullString
	OpenclawToken   sql.NullString
	TunnelToken     sql.NullString
	LastActivityAt  sql.NullTime
	CreatedAt       time.Time
	PausedAt        sql.NullTime
	LastHeartbeatAt sql.NullTime
	CPU         *int
	Memory      *int64
	IdleTimeout *int
}

func (db *DB) CreateSandbox(id, workspaceID, name, sandboxType, sandboxName, opencodeToken, proxyToken, openclawToken, shortID string, cpu int, memory int64, idleTimeout *int) error {
	_, err := db.Exec(
		`INSERT INTO sandboxes (id, workspace_id, name, type, status, sandbox_name, proxy_token, opencode_token, openclaw_token, short_id, last_activity_at, cpu, memory, idle_timeout)
		 VALUES ($1, $2, $3, $4, 'creating', $5, $6, $7, $8, $9, NOW(), $10, $11, $12)`,
		id, workspaceID, name, sandboxType, sandboxName, proxyToken, nullIfEmpty(opencodeToken), nullIfEmpty(openclawToken), nullIfEmpty(shortID), cpu, memory, idleTimeout,
	)
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	return nil
}

// sandboxColumns is the list of columns selected for sandbox queries.
const sandboxColumns = `id, workspace_id, name, type, status, is_local, short_id, sandbox_name, pod_ip, proxy_token, opencode_token, openclaw_token, tunnel_token, last_activity_at, created_at, paused_at, last_heartbeat_at, cpu, memory, idle_timeout`

func scanSandbox(scanner interface{ Scan(...interface{}) error }) (*Sandbox, error) {
	s := &Sandbox{}
	err := scanner.Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.Type, &s.Status, &s.IsLocal, &s.ShortID, &s.SandboxName, &s.PodIP, &s.ProxyToken, &s.OpencodeToken, &s.OpenclawToken, &s.TunnelToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt, &s.LastHeartbeatAt, &s.CPU, &s.Memory, &s.IdleTimeout)
	return s, err
}

func (db *DB) GetSandbox(id string) (*Sandbox, error) {
	s, err := scanSandbox(db.QueryRow(
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE id = $1`, id,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	return s, nil
}

func (db *DB) GetSandboxByShortID(shortID string) (*Sandbox, error) {
	s, err := scanSandbox(db.QueryRow(
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE LOWER(short_id) = LOWER($1)`, shortID,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sandbox by short id: %w", err)
	}
	return s, nil
}

func (db *DB) ListSandboxesByWorkspace(workspaceID string) ([]*Sandbox, error) {
	rows, err := db.Query(
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE workspace_id = $1 ORDER BY created_at ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []*Sandbox
	for rows.Next() {
		s, err := scanSandbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sandbox: %w", err)
		}
		sandboxes = append(sandboxes, s)
	}
	return sandboxes, rows.Err()
}

func (db *DB) DeleteSandbox(id string) error {
	_, err := db.Exec("DELETE FROM sandboxes WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete sandbox: %w", err)
	}
	return nil
}

func (db *DB) UpdateSandboxStatus(id, status string) error {
	var query string
	switch status {
	case "paused":
		query = "UPDATE sandboxes SET status = $2, paused_at = NOW() WHERE id = $1"
	case "running":
		query = "UPDATE sandboxes SET status = $2, paused_at = NULL WHERE id = $1"
	default:
		query = "UPDATE sandboxes SET status = $2 WHERE id = $1"
	}
	_, err := db.Exec(query, id, status)
	if err != nil {
		return fmt.Errorf("update sandbox status: %w", err)
	}
	return nil
}

func (db *DB) UpdateSandboxActivity(id string) error {
	_, err := db.Exec("UPDATE sandboxes SET last_activity_at = NOW() WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("update sandbox activity: %w", err)
	}
	return nil
}

func (db *DB) UpdateSandboxPodIP(id, podIP string) error {
	var err error
	if podIP == "" {
		_, err = db.Exec("UPDATE sandboxes SET pod_ip = NULL WHERE id = $1", id)
	} else {
		_, err = db.Exec("UPDATE sandboxes SET pod_ip = $2 WHERE id = $1", id, podIP)
	}
	if err != nil {
		return fmt.Errorf("update sandbox pod ip: %w", err)
	}
	return nil
}

func (db *DB) UpdateSandboxSandboxName(id, sandboxName string) error {
	_, err := db.Exec("UPDATE sandboxes SET sandbox_name = $2 WHERE id = $1", id, sandboxName)
	if err != nil {
		return fmt.Errorf("update sandbox sandbox name: %w", err)
	}
	return nil
}

func (db *DB) ListIdleSandboxes(defaultTimeoutSeconds int) ([]*Sandbox, error) {
	rows, err := db.Query(
		`SELECT `+sandboxColumns+`
		 FROM sandboxes
		 WHERE status = 'running' AND is_local = FALSE
		   AND COALESCE(idle_timeout, $1) > 0
		   AND last_activity_at < NOW() - (COALESCE(idle_timeout, $1) || ' seconds')::interval`,
		defaultTimeoutSeconds,
	)
	if err != nil {
		return nil, fmt.Errorf("list idle sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []*Sandbox
	for rows.Next() {
		s, err := scanSandbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan idle sandbox: %w", err)
		}
		sandboxes = append(sandboxes, s)
	}
	return sandboxes, rows.Err()
}

func (db *DB) ListAllActiveSandboxNames() ([]string, error) {
	rows, err := db.Query(
		`SELECT sandbox_name FROM sandboxes WHERE sandbox_name IS NOT NULL AND status != 'deleting'`,
	)
	if err != nil {
		return nil, fmt.Errorf("list active sandbox names: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan sandbox name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (db *DB) GetSandboxByProxyToken(proxyToken string) (*Sandbox, error) {
	s, err := scanSandbox(db.QueryRow(
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE proxy_token = $1`, proxyToken,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sandbox by proxy token: %w", err)
	}
	return s, nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// CreateLocalSandbox inserts a local agent sandbox with is_local=true.
func (db *DB) CreateLocalSandbox(id, workspaceID, name, sandboxType, opencodeToken, proxyToken, tunnelToken, shortID string) error {
	_, err := db.Exec(
		`INSERT INTO sandboxes (id, workspace_id, name, type, status, is_local, opencode_token, proxy_token, tunnel_token, short_id, last_activity_at, last_heartbeat_at)
		 VALUES ($1, $2, $3, $4, 'running', TRUE, $5, $6, $7, $8, NOW(), NOW())`,
		id, workspaceID, name, sandboxType, opencodeToken, proxyToken, tunnelToken, nullIfEmpty(shortID),
	)
	if err != nil {
		return fmt.Errorf("create local sandbox: %w", err)
	}
	return nil
}

// UpdateSandboxHeartbeat updates the last_heartbeat_at timestamp.
func (db *DB) UpdateSandboxHeartbeat(id string) error {
	_, err := db.Exec("UPDATE sandboxes SET last_heartbeat_at = NOW() WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("update sandbox heartbeat: %w", err)
	}
	return nil
}

// GetSandboxByTunnelToken finds a local sandbox by its tunnel token.
func (db *DB) GetSandboxByTunnelToken(sandboxID, tunnelToken string) (*Sandbox, error) {
	s, err := scanSandbox(db.QueryRow(
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE id = $1 AND tunnel_token = $2 AND is_local = TRUE`,
		sandboxID, tunnelToken,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sandbox by tunnel token: %w", err)
	}
	return s, nil
}

// --- Agent Registration Codes ---

type AgentRegistrationCode struct {
	Code        string
	UserID      string
	WorkspaceID string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Used        bool
}

// CreateAgentRegistrationCode inserts a new one-time registration code.
func (db *DB) CreateAgentRegistrationCode(code, userID, workspaceID string, expiresAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO agent_registration_codes (code, user_id, workspace_id, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		code, userID, workspaceID, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("create agent registration code: %w", err)
	}
	return nil
}

// ConsumeAgentRegistrationCode atomically validates and marks a code as used.
// Returns the code record if valid, nil if not found/expired/used.
func (db *DB) ConsumeAgentRegistrationCode(code string) (*AgentRegistrationCode, error) {
	arc := &AgentRegistrationCode{}
	err := db.QueryRow(
		`UPDATE agent_registration_codes
		 SET used = TRUE
		 WHERE code = $1 AND used = FALSE AND expires_at > NOW()
		 RETURNING code, user_id, workspace_id, created_at, expires_at, used`,
		code,
	).Scan(&arc.Code, &arc.UserID, &arc.WorkspaceID, &arc.CreatedAt, &arc.ExpiresAt, &arc.Used)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume agent registration code: %w", err)
	}
	return arc, nil
}

func (db *DB) ListAllSandboxes() ([]*Sandbox, error) {
	rows, err := db.Query(
		`SELECT ` + sandboxColumns + ` FROM sandboxes ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []*Sandbox
	for rows.Next() {
		s, err := scanSandbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sandbox: %w", err)
		}
		sandboxes = append(sandboxes, s)
	}
	return sandboxes, rows.Err()
}
