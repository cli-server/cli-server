package codexauth

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/agentserver/agentserver/internal/db"
)

// Store is a thin DB facade over all codex_* tables. Each method takes
// a context and returns a typed value or error; no business logic lives
// here (compose in pkce.go, agent_identity.go, etc.).
type Store struct {
	db *db.DB
}

func NewStore(d *db.DB) *Store {
	return &Store{db: d}
}

// ----- JWKS keys -----

type JwksKey struct {
	Kid          string
	PublicN      string
	PublicE      string
	PrivatePKCS8 []byte
	Active       bool
}

func (s *Store) InsertJwksKey(ctx context.Context, kid string, kp *RSAKeyPair, active bool) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_jwks_keys (kid, public_n, public_e, private_pkcs8, active)
		VALUES ($1, $2, $3, $4, $5)`,
		kid, kp.PublicN, kp.PublicE, kp.PrivatePKCS8, active)
	if err != nil {
		return fmt.Errorf("insert jwks key: %w", err)
	}
	return nil
}

func (s *Store) GetActiveJwksKey(ctx context.Context) (*JwksKey, error) {
	var k JwksKey
	err := s.db.QueryRowContext(ctx, `
		SELECT kid, public_n, public_e, private_pkcs8, active
		FROM codex_jwks_keys WHERE active = TRUE`).
		Scan(&k.Kid, &k.PublicN, &k.PublicE, &k.PrivatePKCS8, &k.Active)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active jwks key: %w", err)
	}
	return &k, nil
}

func (s *Store) ListAllJwksKeys(ctx context.Context) ([]JwksKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kid, public_n, public_e, private_pkcs8, active
		FROM codex_jwks_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list jwks keys: %w", err)
	}
	defer rows.Close()
	var out []JwksKey
	for rows.Next() {
		var k JwksKey
		if err := rows.Scan(&k.Kid, &k.PublicN, &k.PublicE, &k.PrivatePKCS8, &k.Active); err != nil {
			return nil, fmt.Errorf("scan jwks key: %w", err)
		}
		out = append(out, k)
	}
	return out, nil
}
