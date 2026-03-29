package db

import "time"

// IMChannel represents a row in the workspace_im_channels table.
type IMChannel struct {
	ID          string
	WorkspaceID string
	Provider    string
	BotID       string
	UserID      string
	BotToken    string
	BaseURL     string
	Cursor      string
	BoundAt     time.Time
}

// CreateIMChannel inserts or updates a workspace IM channel record.
// On conflict (same workspace+provider+bot), updates bound_at.
// Returns the channel ID.
func (db *DB) CreateIMChannel(workspaceID, provider, botID, userID string) (string, error) {
	var id string
	err := db.QueryRow(
		`INSERT INTO workspace_im_channels (workspace_id, provider, bot_id, user_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workspace_id, provider, bot_id)
		DO UPDATE SET bound_at = NOW()
		RETURNING id`,
		workspaceID, provider, botID, userID,
	).Scan(&id)
	return id, err
}

// SaveIMChannelCredentials stores bot credentials for a workspace IM channel.
func (db *DB) SaveIMChannelCredentials(channelID, botToken, baseURL string) error {
	_, err := db.Exec(
		`UPDATE workspace_im_channels SET bot_token = $1, base_url = $2 WHERE id = $3`,
		botToken, baseURL, channelID,
	)
	return err
}

// GetIMChannel retrieves a single workspace IM channel by ID.
func (db *DB) GetIMChannel(channelID string) (*IMChannel, error) {
	c := &IMChannel{}
	var botToken, baseURL, cursor *string
	err := db.QueryRow(
		`SELECT id, workspace_id, provider, bot_id, user_id, bot_token, base_url, cursor, bound_at
		FROM workspace_im_channels WHERE id = $1`,
		channelID,
	).Scan(&c.ID, &c.WorkspaceID, &c.Provider, &c.BotID, &c.UserID, &botToken, &baseURL, &cursor, &c.BoundAt)
	if err != nil {
		return nil, err
	}
	if botToken != nil {
		c.BotToken = *botToken
	}
	if baseURL != nil {
		c.BaseURL = *baseURL
	}
	if cursor != nil {
		c.Cursor = *cursor
	}
	return c, nil
}

// ListIMChannels returns all IM channels for a workspace.
func (db *DB) ListIMChannels(workspaceID string) ([]IMChannel, error) {
	rows, err := db.Query(
		`SELECT id, workspace_id, provider, bot_id, user_id, bot_token, base_url, cursor, bound_at
		FROM workspace_im_channels WHERE workspace_id = $1 ORDER BY bound_at`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []IMChannel
	for rows.Next() {
		var c IMChannel
		var botToken, baseURL, cursor *string
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Provider, &c.BotID, &c.UserID, &botToken, &baseURL, &cursor, &c.BoundAt); err != nil {
			return nil, err
		}
		if botToken != nil {
			c.BotToken = *botToken
		}
		if baseURL != nil {
			c.BaseURL = *baseURL
		}
		if cursor != nil {
			c.Cursor = *cursor
		}
		channels = append(channels, c)
	}
	return channels, rows.Err()
}

// ListAllActiveChannels returns all IM channels with credentials for a given provider.
// Used by RestoreIMBridgePollers.
func (db *DB) ListAllActiveChannels(provider string) ([]IMChannel, error) {
	rows, err := db.Query(
		`SELECT id, workspace_id, provider, bot_id, user_id, bot_token, base_url, cursor, bound_at
		FROM workspace_im_channels
		WHERE provider = $1 AND bot_token IS NOT NULL`,
		provider,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []IMChannel
	for rows.Next() {
		var c IMChannel
		var botToken, baseURL, cursor *string
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Provider, &c.BotID, &c.UserID, &botToken, &baseURL, &cursor, &c.BoundAt); err != nil {
			return nil, err
		}
		if botToken != nil {
			c.BotToken = *botToken
		}
		if baseURL != nil {
			c.BaseURL = *baseURL
		}
		if cursor != nil {
			c.Cursor = *cursor
		}
		channels = append(channels, c)
	}
	return channels, rows.Err()
}

// DeleteIMChannel deletes a workspace IM channel by ID.
func (db *DB) DeleteIMChannel(channelID string) error {
	_, err := db.Exec(
		`DELETE FROM workspace_im_channels WHERE id = $1`,
		channelID,
	)
	return err
}

// UpdateIMChannelCursor persists the long-poll cursor for an IM channel.
func (db *DB) UpdateIMChannelCursor(channelID, cursor string) error {
	_, err := db.Exec(
		`UPDATE workspace_im_channels SET cursor = $1 WHERE id = $2`,
		cursor, channelID,
	)
	return err
}

// UpsertChannelMeta inserts or updates a channel-specific metadata entry.
func (db *DB) UpsertChannelMeta(channelID, userID, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO workspace_im_channel_meta (channel_id, user_id, meta_key, meta_value, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (channel_id, user_id, meta_key)
		DO UPDATE SET meta_value = $4, updated_at = NOW()`,
		channelID, userID, key, value,
	)
	return err
}

// GetChannelMeta retrieves a channel-specific metadata value.
func (db *DB) GetChannelMeta(channelID, userID, key string) (string, error) {
	var value string
	err := db.QueryRow(
		`SELECT meta_value FROM workspace_im_channel_meta WHERE channel_id = $1 AND user_id = $2 AND meta_key = $3`,
		channelID, userID, key,
	).Scan(&value)
	return value, err
}

// GetAllChannelMeta retrieves all metadata entries for a user on a channel.
func (db *DB) GetAllChannelMeta(channelID, userID string) (map[string]string, error) {
	rows, err := db.Query(
		`SELECT meta_key, meta_value FROM workspace_im_channel_meta WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	meta := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		meta[k] = v
	}
	return meta, rows.Err()
}

// BindSandboxToChannel binds a sandbox to a workspace IM channel.
// Any other sandbox previously bound to this channel is unbound first.
func (db *DB) BindSandboxToChannel(sandboxID, channelID string) error {
	_, err := db.Exec(
		`UPDATE sandboxes SET im_channel_id = NULL WHERE im_channel_id = $1`,
		channelID,
	)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`UPDATE sandboxes SET im_channel_id = $1 WHERE id = $2`,
		channelID, sandboxID,
	)
	return err
}

// UnbindSandboxFromChannel removes the IM channel binding from a sandbox.
func (db *DB) UnbindSandboxFromChannel(sandboxID string) error {
	_, err := db.Exec(
		`UPDATE sandboxes SET im_channel_id = NULL WHERE id = $1`,
		sandboxID,
	)
	return err
}

// GetSandboxForChannel returns the running sandbox bound to a channel.
// Returns sql.ErrNoRows if no sandbox is bound or none is running.
func (db *DB) GetSandboxForChannel(channelID string) (sandboxID, podIP, bridgeSecret string, err error) {
	err = db.QueryRow(
		`SELECT id, pod_ip, nanoclaw_bridge_secret FROM sandboxes
		WHERE im_channel_id = $1 AND status = 'running' AND pod_ip != ''`,
		channelID,
	).Scan(&sandboxID, &podIP, &bridgeSecret)
	return
}

// GetIMChannelForSandbox returns the IM channel bound to a sandbox, if any.
// Returns sql.ErrNoRows if the sandbox has no channel bound.
func (db *DB) GetIMChannelForSandbox(sandboxID string) (*IMChannel, error) {
	c := &IMChannel{}
	var botToken, baseURL, cursor *string
	err := db.QueryRow(
		`SELECT c.id, c.workspace_id, c.provider, c.bot_id, c.user_id, c.bot_token, c.base_url, c.cursor, c.bound_at
		FROM workspace_im_channels c
		JOIN sandboxes s ON s.im_channel_id = c.id
		WHERE s.id = $1`,
		sandboxID,
	).Scan(&c.ID, &c.WorkspaceID, &c.Provider, &c.BotID, &c.UserID, &botToken, &baseURL, &cursor, &c.BoundAt)
	if err != nil {
		return nil, err
	}
	if botToken != nil {
		c.BotToken = *botToken
	}
	if baseURL != nil {
		c.BaseURL = *baseURL
	}
	if cursor != nil {
		c.Cursor = *cursor
	}
	return c, nil
}
