package db

import (
	"database/sql"
	"fmt"
	"time"
)

func (db *DB) CreateToken(token, userID string, expiresAt time.Time) error {
	_, err := db.Exec(
		"INSERT INTO auth_tokens (token, user_id, expires_at) VALUES ($1, $2, $3)",
		token, userID, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	return nil
}

func (db *DB) ValidateToken(token string) (string, error) {
	var userID string
	err := db.QueryRow(
		"SELECT user_id FROM auth_tokens WHERE token = $1 AND expires_at > NOW()",
		token,
	).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("validate token: %w", err)
	}
	return userID, nil
}

func (db *DB) DeleteExpiredTokens() error {
	_, err := db.Exec("DELETE FROM auth_tokens WHERE expires_at < NOW()")
	if err != nil {
		return fmt.Errorf("delete expired tokens: %w", err)
	}
	return nil
}

func (db *DB) SetUserDrive(userID, pvcName string) error {
	_, err := db.Exec(
		`INSERT INTO user_drives (user_id, pvc_name) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET pvc_name = $2`,
		userID, pvcName,
	)
	if err != nil {
		return fmt.Errorf("set user drive: %w", err)
	}
	return nil
}

func (db *DB) GetUserDrive(userID string) (string, error) {
	var pvcName string
	err := db.QueryRow("SELECT pvc_name FROM user_drives WHERE user_id = $1", userID).Scan(&pvcName)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get user drive: %w", err)
	}
	return pvcName, nil
}
