package db

import (
	"fmt"
	"time"
)

// WeixinBinding records a WeChat QR scan binding for a sandbox.
type WeixinBinding struct {
	ID        int
	SandboxID string
	BotID     string
	UserID    string
	BoundAt   time.Time
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
