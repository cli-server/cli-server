package db

import (
	"database/sql"
	"fmt"
	"time"
)

type UserQuota struct {
	UserID                   string
	MaxWorkspaces            *int
	MaxSandboxesPerWorkspace *int
	WorkspaceDriveSize       *string
	SandboxCPU               *string
	SandboxMemory            *string
	IdleTimeout              *string
	WsMaxTotalCPU            *string
	WsMaxTotalMemory         *string
	WsMaxIdleTimeout         *string
	UpdatedAt                time.Time
}

type WorkspaceQuota struct {
	WorkspaceID  string
	MaxSandboxes *int
	SandboxCPU   *string
	SandboxMemory *string
	IdleTimeout  *string
	MaxTotalCPU  *string
	MaxTotalMemory *string
	DriveSize    *string
	UpdatedAt    time.Time
}

func (db *DB) GetSystemSetting(key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM system_settings WHERE key = $1", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get system setting %s: %w", key, err)
	}
	return value, nil
}

func (db *DB) SetSystemSetting(key, value string) error {
	_, err := db.Exec(
		`INSERT INTO system_settings (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("set system setting %s: %w", key, err)
	}
	return nil
}

func (db *DB) GetUserQuota(userID string) (*UserQuota, error) {
	q := &UserQuota{}
	err := db.QueryRow(
		`SELECT user_id, max_workspaces, max_sandboxes_per_workspace,
		        workspace_drive_size, sandbox_cpu, sandbox_memory, idle_timeout,
		        ws_max_total_cpu, ws_max_total_memory, ws_max_idle_timeout,
		        updated_at
		 FROM user_quotas WHERE user_id = $1`,
		userID,
	).Scan(&q.UserID, &q.MaxWorkspaces, &q.MaxSandboxesPerWorkspace,
		&q.WorkspaceDriveSize, &q.SandboxCPU, &q.SandboxMemory, &q.IdleTimeout,
		&q.WsMaxTotalCPU, &q.WsMaxTotalMemory, &q.WsMaxIdleTimeout,
		&q.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user quota: %w", err)
	}
	return q, nil
}

func (db *DB) SetUserQuota(userID string, maxWorkspaces *int, maxSandboxesPerWorkspace *int,
	workspaceDriveSize, sandboxCPU, sandboxMemory, idleTimeout,
	wsMaxTotalCPU, wsMaxTotalMemory, wsMaxIdleTimeout *string) error {
	_, err := db.Exec(
		`INSERT INTO user_quotas (user_id, max_workspaces, max_sandboxes_per_workspace,
		   workspace_drive_size, sandbox_cpu, sandbox_memory, idle_timeout,
		   ws_max_total_cpu, ws_max_total_memory, ws_max_idle_timeout, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		 ON CONFLICT (user_id) DO UPDATE SET
		   max_workspaces = EXCLUDED.max_workspaces,
		   max_sandboxes_per_workspace = EXCLUDED.max_sandboxes_per_workspace,
		   workspace_drive_size = EXCLUDED.workspace_drive_size,
		   sandbox_cpu = EXCLUDED.sandbox_cpu,
		   sandbox_memory = EXCLUDED.sandbox_memory,
		   idle_timeout = EXCLUDED.idle_timeout,
		   ws_max_total_cpu = EXCLUDED.ws_max_total_cpu,
		   ws_max_total_memory = EXCLUDED.ws_max_total_memory,
		   ws_max_idle_timeout = EXCLUDED.ws_max_idle_timeout,
		   updated_at = NOW()`,
		userID, maxWorkspaces, maxSandboxesPerWorkspace,
		workspaceDriveSize, sandboxCPU, sandboxMemory, idleTimeout,
		wsMaxTotalCPU, wsMaxTotalMemory, wsMaxIdleTimeout,
	)
	if err != nil {
		return fmt.Errorf("set user quota: %w", err)
	}
	return nil
}

func (db *DB) DeleteUserQuota(userID string) error {
	_, err := db.Exec("DELETE FROM user_quotas WHERE user_id = $1", userID)
	if err != nil {
		return fmt.Errorf("delete user quota: %w", err)
	}
	return nil
}

func (db *DB) CountWorkspacesOwnedByUser(userID string) (int, error) {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM workspace_members WHERE user_id = $1 AND role = 'owner'",
		userID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count workspaces owned by user: %w", err)
	}
	return count, nil
}

func (db *DB) CountSandboxesByWorkspace(workspaceID string) (int, error) {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM sandboxes WHERE workspace_id = $1",
		workspaceID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count sandboxes by workspace: %w", err)
	}
	return count, nil
}

// SumWorkspaceSandboxResources returns the total CPU (millicores) and memory (bytes)
// allocated by non-offline sandboxes in a workspace.
func (db *DB) SumWorkspaceSandboxResources(workspaceID string) (cpuMillis int64, memBytes int64, err error) {
	err = db.QueryRow(
		`SELECT COALESCE(SUM(cpu_millicores),0), COALESCE(SUM(memory_bytes),0)
		 FROM sandboxes WHERE workspace_id = $1 AND status != 'offline'`,
		workspaceID,
	).Scan(&cpuMillis, &memBytes)
	if err != nil {
		return 0, 0, fmt.Errorf("sum workspace sandbox resources: %w", err)
	}
	return cpuMillis, memBytes, nil
}

func (db *DB) GetWorkspaceQuota(workspaceID string) (*WorkspaceQuota, error) {
	q := &WorkspaceQuota{}
	err := db.QueryRow(
		`SELECT workspace_id, max_sandboxes, sandbox_cpu, sandbox_memory, idle_timeout,
		        max_total_cpu, max_total_memory, drive_size, updated_at
		 FROM workspace_quotas WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&q.WorkspaceID, &q.MaxSandboxes, &q.SandboxCPU, &q.SandboxMemory, &q.IdleTimeout,
		&q.MaxTotalCPU, &q.MaxTotalMemory, &q.DriveSize, &q.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace quota: %w", err)
	}
	return q, nil
}

func (db *DB) SetWorkspaceQuota(workspaceID string, maxSandboxes *int,
	sandboxCPU, sandboxMemory, idleTimeout, maxTotalCPU, maxTotalMemory, driveSize *string) error {
	_, err := db.Exec(
		`INSERT INTO workspace_quotas (workspace_id, max_sandboxes, sandbox_cpu, sandbox_memory,
		   idle_timeout, max_total_cpu, max_total_memory, drive_size, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		 ON CONFLICT (workspace_id) DO UPDATE SET
		   max_sandboxes = EXCLUDED.max_sandboxes,
		   sandbox_cpu = EXCLUDED.sandbox_cpu,
		   sandbox_memory = EXCLUDED.sandbox_memory,
		   idle_timeout = EXCLUDED.idle_timeout,
		   max_total_cpu = EXCLUDED.max_total_cpu,
		   max_total_memory = EXCLUDED.max_total_memory,
		   drive_size = EXCLUDED.drive_size,
		   updated_at = NOW()`,
		workspaceID, maxSandboxes, sandboxCPU, sandboxMemory, idleTimeout,
		maxTotalCPU, maxTotalMemory, driveSize,
	)
	if err != nil {
		return fmt.Errorf("set workspace quota: %w", err)
	}
	return nil
}

func (db *DB) DeleteWorkspaceQuota(workspaceID string) error {
	_, err := db.Exec("DELETE FROM workspace_quotas WHERE workspace_id = $1", workspaceID)
	if err != nil {
		return fmt.Errorf("delete workspace quota: %w", err)
	}
	return nil
}
