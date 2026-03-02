package db

import (
	"database/sql"
	"fmt"
)

// GetPasswordHash returns the bcrypt hash for the given user, or nil if no credential exists.
func (db *DB) GetPasswordHash(userID string) (*string, error) {
	var hash string
	err := db.QueryRow(
		"SELECT password_hash FROM user_credentials WHERE user_id = $1", userID,
	).Scan(&hash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get password hash: %w", err)
	}
	return &hash, nil
}

// SetPasswordHash upserts a password hash for the given user.
func (db *DB) SetPasswordHash(userID, hash string) error {
	_, err := db.Exec(
		`INSERT INTO user_credentials (user_id, password_hash, updated_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (user_id) DO UPDATE SET password_hash = $2, updated_at = NOW()`,
		userID, hash,
	)
	if err != nil {
		return fmt.Errorf("set password hash: %w", err)
	}
	return nil
}
