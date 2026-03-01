package db

import (
	"database/sql"
	"fmt"
	"time"
)

type Workspace struct {
	ID           string
	Name         string
	DiskPVCName  sql.NullString
	K8sNamespace sql.NullString
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type WorkspaceMember struct {
	WorkspaceID string
	UserID      string
	Role        string
	CreatedAt   time.Time
}

func (db *DB) CreateWorkspace(id, name string) error {
	_, err := db.Exec(
		`INSERT INTO workspaces (id, name) VALUES ($1, $2)`,
		id, name,
	)
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	return nil
}

func (db *DB) GetWorkspace(id string) (*Workspace, error) {
	w := &Workspace{}
	err := db.QueryRow(
		`SELECT id, name, disk_pvc_name, k8s_namespace, created_at, updated_at FROM workspaces WHERE id = $1`,
		id,
	).Scan(&w.ID, &w.Name, &w.DiskPVCName, &w.K8sNamespace, &w.CreatedAt, &w.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace: %w", err)
	}
	return w, nil
}

func (db *DB) DeleteWorkspace(id string) error {
	_, err := db.Exec("DELETE FROM workspaces WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	return nil
}

func (db *DB) UpdateWorkspaceDiskPVC(id, pvcName string) error {
	_, err := db.Exec(
		"UPDATE workspaces SET disk_pvc_name = $2, updated_at = NOW() WHERE id = $1",
		id, pvcName,
	)
	if err != nil {
		return fmt.Errorf("update workspace disk pvc: %w", err)
	}
	return nil
}

func (db *DB) ListWorkspacesByUser(userID string) ([]*Workspace, error) {
	rows, err := db.Query(
		`SELECT w.id, w.name, w.disk_pvc_name, w.k8s_namespace, w.created_at, w.updated_at
		 FROM workspaces w
		 JOIN workspace_members wm ON w.id = wm.workspace_id
		 WHERE wm.user_id = $1
		 ORDER BY w.created_at ASC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspaces by user: %w", err)
	}
	defer rows.Close()

	var workspaces []*Workspace
	for rows.Next() {
		w := &Workspace{}
		if err := rows.Scan(&w.ID, &w.Name, &w.DiskPVCName, &w.K8sNamespace, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

func (db *DB) AddWorkspaceMember(workspaceID, userID, role string) error {
	_, err := db.Exec(
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		workspaceID, userID, role,
	)
	if err != nil {
		return fmt.Errorf("add workspace member: %w", err)
	}
	return nil
}

func (db *DB) RemoveWorkspaceMember(workspaceID, userID string) error {
	_, err := db.Exec(
		"DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2",
		workspaceID, userID,
	)
	if err != nil {
		return fmt.Errorf("remove workspace member: %w", err)
	}
	return nil
}

func (db *DB) UpdateWorkspaceMemberRole(workspaceID, userID, role string) error {
	_, err := db.Exec(
		"UPDATE workspace_members SET role = $3 WHERE workspace_id = $1 AND user_id = $2",
		workspaceID, userID, role,
	)
	if err != nil {
		return fmt.Errorf("update workspace member role: %w", err)
	}
	return nil
}

func (db *DB) GetWorkspaceMember(workspaceID, userID string) (*WorkspaceMember, error) {
	m := &WorkspaceMember{}
	err := db.QueryRow(
		`SELECT workspace_id, user_id, role, created_at FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID,
	).Scan(&m.WorkspaceID, &m.UserID, &m.Role, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace member: %w", err)
	}
	return m, nil
}

func (db *DB) ListWorkspaceMembers(workspaceID string) ([]*WorkspaceMember, error) {
	rows, err := db.Query(
		`SELECT workspace_id, user_id, role, created_at FROM workspace_members WHERE workspace_id = $1 ORDER BY created_at ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspace members: %w", err)
	}
	defer rows.Close()

	var members []*WorkspaceMember
	for rows.Next() {
		m := &WorkspaceMember{}
		if err := rows.Scan(&m.WorkspaceID, &m.UserID, &m.Role, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace member: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func (db *DB) IsWorkspaceMember(workspaceID, userID string) (bool, error) {
	var exists bool
	err := db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM workspace_members WHERE workspace_id = $1 AND user_id = $2)",
		workspaceID, userID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check workspace membership: %w", err)
	}
	return exists, nil
}

func (db *DB) GetWorkspaceMemberRole(workspaceID, userID string) (string, error) {
	var role string
	err := db.QueryRow(
		"SELECT role FROM workspace_members WHERE workspace_id = $1 AND user_id = $2",
		workspaceID, userID,
	).Scan(&role)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get workspace member role: %w", err)
	}
	return role, nil
}

func (db *DB) SetWorkspaceNamespace(id, namespace string) error {
	_, err := db.Exec(
		"UPDATE workspaces SET k8s_namespace = $2, updated_at = NOW() WHERE id = $1",
		id, namespace,
	)
	if err != nil {
		return fmt.Errorf("set workspace namespace: %w", err)
	}
	return nil
}

func (db *DB) GetAllWorkspaceNamespaces() ([]string, error) {
	rows, err := db.Query(
		`SELECT DISTINCT k8s_namespace FROM workspaces WHERE k8s_namespace IS NOT NULL AND k8s_namespace != ''`,
	)
	if err != nil {
		return nil, fmt.Errorf("get all workspace namespaces: %w", err)
	}
	defer rows.Close()

	var namespaces []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, fmt.Errorf("scan workspace namespace: %w", err)
		}
		namespaces = append(namespaces, ns)
	}
	return namespaces, rows.Err()
}

func (db *DB) ListWorkspacesWithoutNamespace() ([]*Workspace, error) {
	rows, err := db.Query(
		`SELECT id, name, disk_pvc_name, k8s_namespace, created_at, updated_at
		 FROM workspaces
		 WHERE k8s_namespace IS NULL OR k8s_namespace = ''`,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspaces without namespace: %w", err)
	}
	defer rows.Close()

	var workspaces []*Workspace
	for rows.Next() {
		w := &Workspace{}
		if err := rows.Scan(&w.ID, &w.Name, &w.DiskPVCName, &w.K8sNamespace, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

func (db *DB) ListAllWorkspaces() ([]*Workspace, error) {
	rows, err := db.Query(
		`SELECT id, name, disk_pvc_name, k8s_namespace, created_at, updated_at
		 FROM workspaces ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all workspaces: %w", err)
	}
	defer rows.Close()

	var workspaces []*Workspace
	for rows.Next() {
		w := &Workspace{}
		if err := rows.Scan(&w.ID, &w.Name, &w.DiskPVCName, &w.K8sNamespace, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}
