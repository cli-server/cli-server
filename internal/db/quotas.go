package db

import (
	"database/sql"
	"fmt"
	"time"
)

type UserQuota struct {
	UserID        string
	MaxWorkspaces *int
	UpdatedAt     time.Time
}

type WorkspaceQuota struct {
	WorkspaceID      string
	MaxSandboxes     *int
	MaxSandboxCPU    *int   // millicores
	MaxSandboxMemory *int64 // bytes
	MaxIdleTimeout   *int   // seconds
	MaxTotalCPU      *int   // millicores
	MaxTotalMemory   *int64 // bytes
	MaxDriveSize     *int64 // bytes
	UpdatedAt        time.Time
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
		`SELECT user_id, max_workspaces, updated_at
		 FROM user_quotas WHERE user_id = $1`,
		userID,
	).Scan(&q.UserID, &q.MaxWorkspaces, &q.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user quota: %w", err)
	}
	return q, nil
}

func (db *DB) SetUserQuota(userID string, maxWorkspaces *int) error {
	_, err := db.Exec(
		`INSERT INTO user_quotas (user_id, max_workspaces, updated_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (user_id) DO UPDATE SET
		   max_workspaces = EXCLUDED.max_workspaces,
		   updated_at = NOW()`,
		userID, maxWorkspaces,
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
		`SELECT workspace_id, max_sandboxes, max_sandbox_cpu, max_sandbox_memory, max_idle_timeout,
		        max_total_cpu, max_total_memory, max_drive_size, updated_at
		 FROM workspace_quotas WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&q.WorkspaceID, &q.MaxSandboxes, &q.MaxSandboxCPU, &q.MaxSandboxMemory, &q.MaxIdleTimeout,
		&q.MaxTotalCPU, &q.MaxTotalMemory, &q.MaxDriveSize, &q.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace quota: %w", err)
	}
	return q, nil
}

func (db *DB) SetWorkspaceQuota(workspaceID string, maxSandboxes *int,
	maxSandboxCPU *int, maxSandboxMemory *int64, maxIdleTimeout *int, maxTotalCPU *int, maxTotalMemory *int64, maxDriveSize *int64) error {
	_, err := db.Exec(
		`INSERT INTO workspace_quotas (workspace_id, max_sandboxes, max_sandbox_cpu, max_sandbox_memory,
		   max_idle_timeout, max_total_cpu, max_total_memory, max_drive_size, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		 ON CONFLICT (workspace_id) DO UPDATE SET
		   max_sandboxes = EXCLUDED.max_sandboxes,
		   max_sandbox_cpu = EXCLUDED.max_sandbox_cpu,
		   max_sandbox_memory = EXCLUDED.max_sandbox_memory,
		   max_idle_timeout = EXCLUDED.max_idle_timeout,
		   max_total_cpu = EXCLUDED.max_total_cpu,
		   max_total_memory = EXCLUDED.max_total_memory,
		   max_drive_size = EXCLUDED.max_drive_size,
		   updated_at = NOW()`,
		workspaceID, maxSandboxes, maxSandboxCPU, maxSandboxMemory, maxIdleTimeout,
		maxTotalCPU, maxTotalMemory, maxDriveSize,
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
