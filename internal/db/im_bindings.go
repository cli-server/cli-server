package db

import "time"

// IMBinding represents a row in the sandbox_im_bindings table.
type IMBinding struct {
	ID        int
	SandboxID string
	Provider  string
	BotID     string
	UserID    string
	BotToken  string
	BaseURL   string
	Cursor    string
	BoundAt   time.Time
}

// CreateIMBinding inserts or updates an IM binding record.
// On conflict (same sandbox+provider+bot), updates user_id and bound_at.
func (db *DB) CreateIMBinding(sandboxID, provider, botID, userID string) error {
	_, err := db.Exec(
		`INSERT INTO sandbox_im_bindings (sandbox_id, provider, bot_id, user_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (sandbox_id, provider, bot_id)
		DO UPDATE SET user_id = EXCLUDED.user_id, bound_at = NOW()`,
		sandboxID, provider, botID, userID,
	)
	return err
}

// SaveIMCredentials stores bot credentials for an IM binding.
func (db *DB) SaveIMCredentials(sandboxID, provider, botID, botToken, baseURL string) error {
	_, err := db.Exec(
		`UPDATE sandbox_im_bindings SET bot_token = $1, base_url = $2 WHERE sandbox_id = $3 AND provider = $4 AND bot_id = $5`,
		botToken, baseURL, sandboxID, provider, botID,
	)
	return err
}

// GetIMCredentials retrieves bot credentials for an IM binding.
func (db *DB) GetIMCredentials(sandboxID, provider, botID string) (botToken, baseURL string, err error) {
	err = db.QueryRow(
		`SELECT COALESCE(bot_token, ''), COALESCE(base_url, '') FROM sandbox_im_bindings WHERE sandbox_id = $1 AND provider = $2 AND bot_id = $3`,
		sandboxID, provider, botID,
	).Scan(&botToken, &baseURL)
	return
}

// ListIMBindings returns all IM bindings for a sandbox, optionally filtered by provider.
// If provider is empty, all bindings are returned.
func (db *DB) ListIMBindings(sandboxID, provider string) ([]*IMBinding, error) {
	query := `SELECT id, sandbox_id, provider, bot_id, user_id, bound_at FROM sandbox_im_bindings WHERE sandbox_id = $1`
	args := []interface{}{sandboxID}
	if provider != "" {
		query += ` AND provider = $2`
		args = append(args, provider)
	}
	query += ` ORDER BY bound_at DESC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []*IMBinding
	for rows.Next() {
		b := &IMBinding{}
		if err := rows.Scan(&b.ID, &b.SandboxID, &b.Provider, &b.BotID, &b.UserID, &b.BoundAt); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// GetActiveBindings returns all bindings with credentials for a given provider,
// filtered to sandboxes of type 'nanoclaw' with status 'running'.
func (db *DB) GetActiveBindings(provider string) ([]*IMBinding, error) {
	rows, err := db.Query(
		`SELECT b.id, b.sandbox_id, b.provider, b.bot_id, b.user_id, b.bot_token, b.base_url, b.cursor, b.bound_at
		FROM sandbox_im_bindings b
		JOIN sandboxes s ON s.id = b.sandbox_id
		WHERE b.provider = $1
		  AND b.bot_token IS NOT NULL AND b.bot_token != ''
		  AND s.type = 'nanoclaw' AND s.status = 'running'`,
		provider,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []*IMBinding
	for rows.Next() {
		b := &IMBinding{}
		var botToken, baseURL, cursor *string
		if err := rows.Scan(&b.ID, &b.SandboxID, &b.Provider, &b.BotID, &b.UserID, &botToken, &baseURL, &cursor, &b.BoundAt); err != nil {
			return nil, err
		}
		if botToken != nil {
			b.BotToken = *botToken
		}
		if baseURL != nil {
			b.BaseURL = *baseURL
		}
		if cursor != nil {
			b.Cursor = *cursor
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// GetActiveBindingsForSandbox returns all bindings with credentials for a specific sandbox.
func (db *DB) GetActiveBindingsForSandbox(sandboxID string) ([]*IMBinding, error) {
	rows, err := db.Query(
		`SELECT id, sandbox_id, provider, bot_id, user_id, bot_token, base_url, cursor, bound_at
		FROM sandbox_im_bindings
		WHERE sandbox_id = $1 AND bot_token IS NOT NULL AND bot_token != ''`,
		sandboxID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []*IMBinding
	for rows.Next() {
		b := &IMBinding{}
		var botToken, baseURL, cursor *string
		if err := rows.Scan(&b.ID, &b.SandboxID, &b.Provider, &b.BotID, &b.UserID, &botToken, &baseURL, &cursor, &b.BoundAt); err != nil {
			return nil, err
		}
		if botToken != nil {
			b.BotToken = *botToken
		}
		if baseURL != nil {
			b.BaseURL = *baseURL
		}
		if cursor != nil {
			b.Cursor = *cursor
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// UpdateCursor persists the long-poll cursor for an IM binding.
func (db *DB) UpdateCursor(sandboxID, provider, botID, cursor string) error {
	_, err := db.Exec(
		`UPDATE sandbox_im_bindings SET cursor = $1, last_poll_at = NOW() WHERE sandbox_id = $2 AND provider = $3 AND bot_id = $4`,
		cursor, sandboxID, provider, botID,
	)
	return err
}

// UpsertProviderMeta inserts or updates a provider-specific metadata entry.
func (db *DB) UpsertProviderMeta(sandboxID, provider, botID, userID, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO im_provider_meta (sandbox_id, provider, bot_id, user_id, meta_key, meta_value, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (sandbox_id, provider, bot_id, user_id, meta_key)
		DO UPDATE SET meta_value = $6, updated_at = NOW()`,
		sandboxID, provider, botID, userID, key, value,
	)
	return err
}

// GetProviderMeta retrieves a provider-specific metadata value.
func (db *DB) GetProviderMeta(sandboxID, provider, botID, userID, key string) (string, error) {
	var value string
	err := db.QueryRow(
		`SELECT meta_value FROM im_provider_meta WHERE sandbox_id = $1 AND provider = $2 AND bot_id = $3 AND user_id = $4 AND meta_key = $5`,
		sandboxID, provider, botID, userID, key,
	).Scan(&value)
	return value, err
}

// GetAllProviderMeta retrieves all metadata entries for a user.
func (db *DB) GetAllProviderMeta(sandboxID, provider, botID, userID string) (map[string]string, error) {
	rows, err := db.Query(
		`SELECT meta_key, meta_value FROM im_provider_meta WHERE sandbox_id = $1 AND provider = $2 AND bot_id = $3 AND user_id = $4`,
		sandboxID, provider, botID, userID,
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

// DeleteIMBinding deletes an IM binding by sandbox, provider, and bot ID.
func (db *DB) DeleteIMBinding(sandboxID, provider, botID string) error {
	_, err := db.Exec(
		`DELETE FROM sandbox_im_bindings WHERE sandbox_id = $1 AND provider = $2 AND bot_id = $3`,
		sandboxID, provider, botID,
	)
	return err
}
