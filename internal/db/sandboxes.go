package db

import (
	"database/sql"
	"fmt"
	"time"
)

type Sandbox struct {
	ID               string
	WorkspaceID      string
	Name             string
	Type             string
	Status           string
	SandboxName      sql.NullString
	PodIP            sql.NullString
	OpencodePassword sql.NullString
	ProxyToken       sql.NullString
	TelegramBotToken sql.NullString
	GatewayToken     sql.NullString
	LastActivityAt   sql.NullTime
	CreatedAt        time.Time
	PausedAt         sql.NullTime
}

func (db *DB) CreateSandbox(id, workspaceID, name, sandboxType, sandboxName, opencodePassword, proxyToken, telegramBotToken, gatewayToken string) error {
	_, err := db.Exec(
		`INSERT INTO sandboxes (id, workspace_id, name, type, status, sandbox_name, opencode_password, proxy_token, telegram_bot_token, gateway_token, last_activity_at)
		 VALUES ($1, $2, $3, $4, 'creating', $5, $6, $7, $8, $9, NOW())`,
		id, workspaceID, name, sandboxType, sandboxName, opencodePassword, proxyToken, nullIfEmpty(telegramBotToken), nullIfEmpty(gatewayToken),
	)
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	return nil
}

func (db *DB) GetSandbox(id string) (*Sandbox, error) {
	s := &Sandbox{}
	err := db.QueryRow(
		`SELECT id, workspace_id, name, type, status, sandbox_name, pod_ip, opencode_password, proxy_token, telegram_bot_token, gateway_token, last_activity_at, created_at, paused_at
		 FROM sandboxes WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.Type, &s.Status, &s.SandboxName, &s.PodIP, &s.OpencodePassword, &s.ProxyToken, &s.TelegramBotToken, &s.GatewayToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	return s, nil
}

func (db *DB) ListSandboxesByWorkspace(workspaceID string) ([]*Sandbox, error) {
	rows, err := db.Query(
		`SELECT id, workspace_id, name, type, status, sandbox_name, pod_ip, opencode_password, proxy_token, telegram_bot_token, gateway_token, last_activity_at, created_at, paused_at
		 FROM sandboxes WHERE workspace_id = $1 ORDER BY created_at ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []*Sandbox
	for rows.Next() {
		s := &Sandbox{}
		if err := rows.Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.Type, &s.Status, &s.SandboxName, &s.PodIP, &s.OpencodePassword, &s.ProxyToken, &s.TelegramBotToken, &s.GatewayToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt); err != nil {
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

func (db *DB) ListIdleSandboxes(idleTimeout time.Duration) ([]*Sandbox, error) {
	rows, err := db.Query(
		`SELECT id, workspace_id, name, type, status, sandbox_name, pod_ip, opencode_password, proxy_token, telegram_bot_token, gateway_token, last_activity_at, created_at, paused_at
		 FROM sandboxes
		 WHERE status = 'running' AND last_activity_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int(idleTimeout.Seconds())),
	)
	if err != nil {
		return nil, fmt.Errorf("list idle sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []*Sandbox
	for rows.Next() {
		s := &Sandbox{}
		if err := rows.Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.Type, &s.Status, &s.SandboxName, &s.PodIP, &s.OpencodePassword, &s.ProxyToken, &s.TelegramBotToken, &s.GatewayToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt); err != nil {
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
	s := &Sandbox{}
	err := db.QueryRow(
		`SELECT id, workspace_id, name, type, status, sandbox_name, pod_ip, opencode_password, proxy_token, telegram_bot_token, gateway_token, last_activity_at, created_at, paused_at
		 FROM sandboxes WHERE proxy_token = $1`,
		proxyToken,
	).Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.Type, &s.Status, &s.SandboxName, &s.PodIP, &s.OpencodePassword, &s.ProxyToken, &s.TelegramBotToken, &s.GatewayToken, &s.LastActivityAt, &s.CreatedAt, &s.PausedAt)
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
