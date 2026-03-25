package db

import (
	"fmt"
	"time"
)

// WeixinBinding records a WeChat QR scan binding for a sandbox.
type WeixinBinding struct {
	ID            int
	SandboxID     string
	BotID         string
	UserID        string
	BoundAt       time.Time
	BotToken      string
	ILinkBaseURL  string
	GetUpdatesBuf string
}

// CreateWeixinBinding inserts a new binding record after a successful QR login.
func (db *DB) CreateWeixinBinding(sandboxID, botID, userID string) error {
	_, err := db.Exec(
		`INSERT INTO sandbox_weixin_bindings (sandbox_id, bot_id, user_id) VALUES ($1, $2, $3)`,
		sandboxID, botID, userID,
	)
	if err != nil {
		return fmt.Errorf("create weixin binding: %w", err)
	}
	return nil
}

// ListWeixinBindings returns all binding records for a sandbox, most recent first.
func (db *DB) ListWeixinBindings(sandboxID string) ([]*WeixinBinding, error) {
	rows, err := db.Query(
		`SELECT id, sandbox_id, bot_id, user_id, bound_at
		 FROM sandbox_weixin_bindings
		 WHERE sandbox_id = $1
		 ORDER BY bound_at DESC`,
		sandboxID,
	)
	if err != nil {
		return nil, fmt.Errorf("list weixin bindings: %w", err)
	}
	defer rows.Close()

	var bindings []*WeixinBinding
	for rows.Next() {
		b := &WeixinBinding{}
		if err := rows.Scan(&b.ID, &b.SandboxID, &b.BotID, &b.UserID, &b.BoundAt); err != nil {
			return nil, fmt.Errorf("scan weixin binding: %w", err)
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// GetSandboxByBotID returns the sandbox_id for a given WeChat bot_id.
// Used for routing inbound iLink messages to the correct NanoClaw sandbox.
func (db *DB) GetSandboxByBotID(botID string) (string, error) {
	var sandboxID string
	err := db.QueryRow(
		`SELECT sandbox_id FROM sandbox_weixin_bindings WHERE bot_id = $1 ORDER BY bound_at DESC LIMIT 1`,
		botID,
	).Scan(&sandboxID)
	if err != nil {
		return "", fmt.Errorf("get sandbox by bot ID: %w", err)
	}
	return sandboxID, nil
}

// SaveBotCredentials stores iLink bot credentials for bridge-mode messaging.
// Used by nanoclaw sandboxes where agentserver holds the credentials.
func (db *DB) SaveBotCredentials(sandboxID, botID, botToken, baseURL string) error {
	_, err := db.Exec(
		`UPDATE sandbox_weixin_bindings SET bot_token = $1, ilink_base_url = $2
		 WHERE sandbox_id = $3 AND bot_id = $4`,
		botToken, baseURL, sandboxID, botID,
	)
	if err != nil {
		return fmt.Errorf("save bot credentials: %w", err)
	}
	return nil
}

// GetBotCredentials returns the bot_token and ilink_base_url for a specific binding.
// Used by the outbound bridge endpoint to send messages via iLink.
func (db *DB) GetBotCredentials(sandboxID, botID string) (botToken, baseURL string, err error) {
	err = db.QueryRow(
		`SELECT COALESCE(bot_token, ''), COALESCE(ilink_base_url, '')
		 FROM sandbox_weixin_bindings
		 WHERE sandbox_id = $1 AND bot_id = $2`,
		sandboxID, botID,
	).Scan(&botToken, &baseURL)
	if err != nil {
		return "", "", fmt.Errorf("get bot credentials: %w", err)
	}
	return botToken, baseURL, nil
}

// GetBindingsWithBotToken returns all nanoclaw bindings that have a bot_token set,
// for starting long-poll goroutines. Only returns bindings for running nanoclaw sandboxes.
func (db *DB) GetBindingsWithBotToken() ([]*WeixinBinding, error) {
	rows, err := db.Query(
		`SELECT b.id, b.sandbox_id, b.bot_id, b.user_id, b.bound_at,
		        COALESCE(b.bot_token, ''), COALESCE(b.ilink_base_url, ''), COALESCE(b.get_updates_buf, '')
		 FROM sandbox_weixin_bindings b
		 JOIN sandboxes s ON s.id = b.sandbox_id
		 WHERE b.bot_token IS NOT NULL AND b.bot_token != ''
		   AND s.type = 'nanoclaw' AND s.status = 'running'
		 ORDER BY b.bound_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("get bindings with bot token: %w", err)
	}
	defer rows.Close()

	var bindings []*WeixinBinding
	for rows.Next() {
		b := &WeixinBinding{}
		if err := rows.Scan(&b.ID, &b.SandboxID, &b.BotID, &b.UserID, &b.BoundAt,
			&b.BotToken, &b.ILinkBaseURL, &b.GetUpdatesBuf); err != nil {
			return nil, fmt.Errorf("scan binding with bot token: %w", err)
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// UpdateGetUpdatesBuf persists the long-poll cursor for a binding.
func (db *DB) UpdateGetUpdatesBuf(sandboxID, botID, buf string) error {
	_, err := db.Exec(
		`UPDATE sandbox_weixin_bindings SET get_updates_buf = $1, last_poll_at = NOW()
		 WHERE sandbox_id = $2 AND bot_id = $3`,
		buf, sandboxID, botID,
	)
	if err != nil {
		return fmt.Errorf("update get_updates_buf: %w", err)
	}
	return nil
}

// UpsertContextToken stores or updates the context_token for a user conversation.
func (db *DB) UpsertContextToken(sandboxID, botID, userID, contextToken string) error {
	_, err := db.Exec(
		`INSERT INTO weixin_context_tokens (sandbox_id, bot_id, user_id, context_token, updated_at)
		 VALUES ($1, $2, $3, $4, NOW())
		 ON CONFLICT (sandbox_id, bot_id, user_id) DO UPDATE SET context_token = $4, updated_at = NOW()`,
		sandboxID, botID, userID, contextToken,
	)
	if err != nil {
		return fmt.Errorf("upsert context token: %w", err)
	}
	return nil
}

// GetContextToken retrieves the cached context_token for a user.
func (db *DB) GetContextToken(sandboxID, botID, userID string) (string, error) {
	var token string
	err := db.QueryRow(
		`SELECT context_token FROM weixin_context_tokens
		 WHERE sandbox_id = $1 AND bot_id = $2 AND user_id = $3`,
		sandboxID, botID, userID,
	).Scan(&token)
	if err != nil {
		return "", err
	}
	return token, nil
}
