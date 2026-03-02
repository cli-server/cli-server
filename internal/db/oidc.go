package db

import (
	"database/sql"
	"fmt"
	"time"
)

type OIDCIdentity struct {
	Provider  string
	Subject   string
	UserID    string
	Email     *string
	CreatedAt time.Time
}

func (db *DB) GetOIDCIdentity(provider, subject string) (*OIDCIdentity, error) {
	oi := &OIDCIdentity{}
	err := db.QueryRow(
		"SELECT provider, subject, user_id, email, created_at FROM oidc_identities WHERE provider = $1 AND subject = $2",
		provider, subject,
	).Scan(&oi.Provider, &oi.Subject, &oi.UserID, &oi.Email, &oi.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get oidc identity: %w", err)
	}
	return oi, nil
}

func (db *DB) CreateOIDCIdentity(provider, subject, userID string, email *string) error {
	_, err := db.Exec(
		"INSERT INTO oidc_identities (provider, subject, user_id, email) VALUES ($1, $2, $3, $4)",
		provider, subject, userID, email,
	)
	if err != nil {
		return fmt.Errorf("create oidc identity: %w", err)
	}
	return nil
}

func (db *DB) UpdateOIDCIdentityEmail(provider, subject, email string) error {
	_, err := db.Exec(
		"UPDATE oidc_identities SET email = $1, updated_at = NOW() WHERE provider = $2 AND subject = $3",
		email, provider, subject,
	)
	if err != nil {
		return fmt.Errorf("update oidc identity email: %w", err)
	}
	return nil
}
