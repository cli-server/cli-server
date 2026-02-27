package db

import (
	"database/sql"
	"fmt"
	"time"
)

type Session struct {
	ID               string
	UserID           string
	Name             string
	Status           string
	SandboxName      sql.NullString
	PodIP            sql.NullString
	OpencodePassword sql.NullString
	ProxyToken       sql.NullString
	LastActivityAt   sql.NullTime
	CreatedAt        time.Time
	PausedAt         sql.NullTime
}

func (db *DB) CreateSession(id, userID, name, sandboxName, opencodePassword, proxyToken string) error {
	_, err := db.Exec(
		`INSERT INTO sessions (id, user_id, name, status, sandbox_name, opencode_password, proxy_token, last_activity_at)
		 VALUES ($1, $2, $3, 'creating', $4, $5, $6, NOW())`,
		id, userID, name, sandboxName, opencodePassword, proxyToken,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (db *DB) GetSession(id string) (*Session, error) {
	s := &Session{}
	err := db.QueryRow(
		`SELECT id, user_id, name, status, sandbox_name, pod_ip, opencode_password, proxy_token, last_activity_at, created_at, paused_at
		 FROM sessions WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.UserID, &s.Name, &s.Status, &s.SandboxName, &s.PodIP, &s.OpencodePassword, &s.ProxyToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

func (db *DB) ListSessionsByUser(userID string) ([]*Session, error) {
	rows, err := db.Query(
		`SELECT id, user_id, name, status, sandbox_name, pod_ip, opencode_password, proxy_token, last_activity_at, created_at, paused_at
		 FROM sessions WHERE user_id = $1 ORDER BY created_at ASC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*Session
	for rows.Next() {
		s := &Session{}
		if err := rows.Scan(&s.ID, &s.UserID, &s.Name, &s.Status, &s.SandboxName, &s.PodIP, &s.OpencodePassword, &s.ProxyToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (db *DB) UpdateSessionStatus(id, status string) error {
	var query string
	switch status {
	case "paused":
		query = "UPDATE sessions SET status = $2, paused_at = NOW() WHERE id = $1"
	case "running":
		query = "UPDATE sessions SET status = $2, paused_at = NULL WHERE id = $1"
	default:
		query = "UPDATE sessions SET status = $2 WHERE id = $1"
	}
	_, err := db.Exec(query, id, status)
	if err != nil {
		return fmt.Errorf("update session status: %w", err)
	}
	return nil
}

func (db *DB) UpdateSessionActivity(id string) error {
	_, err := db.Exec("UPDATE sessions SET last_activity_at = NOW() WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("update session activity: %w", err)
	}
	return nil
}

func (db *DB) DeleteSession(id string) error {
	_, err := db.Exec("DELETE FROM sessions WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (db *DB) ListIdleSessions(idleTimeout time.Duration) ([]*Session, error) {
	rows, err := db.Query(
		`SELECT id, user_id, name, status, sandbox_name, pod_ip, opencode_password, proxy_token, last_activity_at, created_at, paused_at
		 FROM sessions
		 WHERE status = 'running' AND last_activity_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int(idleTimeout.Seconds())),
	)
	if err != nil {
		return nil, fmt.Errorf("list idle sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*Session
	for rows.Next() {
		s := &Session{}
		if err := rows.Scan(&s.ID, &s.UserID, &s.Name, &s.Status, &s.SandboxName, &s.PodIP, &s.OpencodePassword, &s.ProxyToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt); err != nil {
			return nil, fmt.Errorf("scan idle session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (db *DB) ListAllActiveSandboxNames() ([]string, error) {
	rows, err := db.Query(
		`SELECT sandbox_name FROM sessions WHERE sandbox_name IS NOT NULL AND status != 'deleting'`,
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

func (db *DB) UpdateSessionSandboxName(id, sandboxName string) error {
	_, err := db.Exec("UPDATE sessions SET sandbox_name = $2 WHERE id = $1", id, sandboxName)
	if err != nil {
		return fmt.Errorf("update session sandbox name: %w", err)
	}
	return nil
}

func (db *DB) UpdateSessionPodIP(id, podIP string) error {
	_, err := db.Exec("UPDATE sessions SET pod_ip = $2 WHERE id = $1", id, podIP)
	if err != nil {
		return fmt.Errorf("update session pod ip: %w", err)
	}
	return nil
}

func (db *DB) GetSessionByProxyToken(proxyToken string) (*Session, error) {
	s := &Session{}
	err := db.QueryRow(
		`SELECT id, user_id, name, status, sandbox_name, pod_ip, opencode_password, proxy_token, last_activity_at, created_at, paused_at
		 FROM sessions WHERE proxy_token = $1`,
		proxyToken,
	).Scan(&s.ID, &s.UserID, &s.Name, &s.Status, &s.SandboxName, &s.PodIP, &s.OpencodePassword, &s.ProxyToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session by proxy token: %w", err)
	}
	return s, nil
}
