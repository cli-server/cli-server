package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type ModelserverConnection struct {
	WorkspaceID    string     `json:"workspace_id"`
	ProjectID      string     `json:"project_id"`
	ProjectName    string     `json:"project_name"`
	UserID         string     `json:"user_id"`
	AccessToken    string     `json:"-"`
	RefreshToken   string     `json:"-"`
	TokenExpiresAt time.Time  `json:"token_expires_at"`
	Models         []LLMModel `json:"models"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (db *DB) GetModelserverConnection(workspaceID string) (*ModelserverConnection, error) {
	c := &ModelserverConnection{}
	var modelsJSON []byte
	err := db.QueryRow(
		`SELECT workspace_id, project_id, project_name, user_id, access_token, refresh_token,
		        token_expires_at, models, created_at, updated_at
		 FROM workspace_modelserver_tokens WHERE workspace_id = $1`,
		workspaceID,
	).Scan(
		&c.WorkspaceID, &c.ProjectID, &c.ProjectName, &c.UserID,
		&c.AccessToken, &c.RefreshToken, &c.TokenExpiresAt,
		&modelsJSON, &c.CreatedAt, &c.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get modelserver connection: %w", err)
	}
	if err := json.Unmarshal(modelsJSON, &c.Models); err != nil {
		return nil, fmt.Errorf("get modelserver connection: unmarshal models: %w", err)
	}
	return c, nil
}

func (db *DB) SetModelserverConnection(c *ModelserverConnection) error {
	modelsJSON, err := json.Marshal(c.Models)
	if err != nil {
		return fmt.Errorf("set modelserver connection: marshal models: %w", err)
	}
	_, err = db.Exec(
		`INSERT INTO workspace_modelserver_tokens
		   (workspace_id, project_id, project_name, user_id, access_token, refresh_token, token_expires_at, models, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		 ON CONFLICT (workspace_id) DO UPDATE SET
		   project_id       = EXCLUDED.project_id,
		   project_name     = EXCLUDED.project_name,
		   user_id          = EXCLUDED.user_id,
		   access_token     = EXCLUDED.access_token,
		   refresh_token    = EXCLUDED.refresh_token,
		   token_expires_at = EXCLUDED.token_expires_at,
		   models           = EXCLUDED.models,
		   updated_at       = NOW()`,
		c.WorkspaceID, c.ProjectID, c.ProjectName, c.UserID,
		c.AccessToken, c.RefreshToken, c.TokenExpiresAt, modelsJSON,
	)
	if err != nil {
		return fmt.Errorf("set modelserver connection: %w", err)
	}
	return nil
}

func (db *DB) DeleteModelserverConnection(workspaceID string) error {
	_, err := db.Exec("DELETE FROM workspace_modelserver_tokens WHERE workspace_id = $1", workspaceID)
	if err != nil {
		return fmt.Errorf("delete modelserver connection: %w", err)
	}
	return nil
}

func (db *DB) UpdateModelserverTokens(workspaceID, accessToken, refreshToken string, expiresAt time.Time) error {
	_, err := db.Exec(
		`UPDATE workspace_modelserver_tokens
		 SET access_token = $2, refresh_token = $3, token_expires_at = $4, updated_at = NOW()
		 WHERE workspace_id = $1`,
		workspaceID, accessToken, refreshToken, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("update modelserver tokens: %w", err)
	}
	return nil
}

func (db *DB) HasModelserverConnection(workspaceID string) (bool, error) {
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM workspace_modelserver_tokens WHERE workspace_id = $1)`,
		workspaceID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("has modelserver connection: %w", err)
	}
	return exists, nil
}
