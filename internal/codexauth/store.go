package codexauth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agentserver/agentserver/internal/db"
)

// ErrRefreshTokenReuse is returned by RotateRefreshToken when the
// presented token is unknown OR has already been revoked. Callers
// (the /oauth/token refresh handler) should map this to a 401 with
// OAuth error `refresh_token_expired` or `refresh_token_reused`.
var ErrRefreshTokenReuse = errors.New("refresh token unknown or revoked")

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

// ----- PKCE -----

type PkceRequest struct {
	Code          string
	CodeChallenge string
	State         string
	UserID        string
	ExpiresAt     time.Time
}

func (s *Store) InsertPkceRequest(ctx context.Context, r PkceRequest) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_pkce_requests (code, code_challenge, state, user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		r.Code, r.CodeChallenge, r.State, r.UserID, r.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert pkce request: %w", err)
	}
	return nil
}

// ConsumePkceRequest atomically deletes and returns the row.
// Returns nil if the code is missing or expired.
func (s *Store) ConsumePkceRequest(ctx context.Context, code string) (*PkceRequest, error) {
	var r PkceRequest
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM codex_pkce_requests
		WHERE code = $1 AND expires_at > NOW()
		RETURNING code, code_challenge, state, user_id, expires_at`,
		code).Scan(&r.Code, &r.CodeChallenge, &r.State, &r.UserID, &r.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume pkce request: %w", err)
	}
	return &r, nil
}

// ----- Tokens -----

// HashToken returns sha256(raw) suitable for DB primary key.
// Tokens are never stored plaintext.
func HashToken(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

func (s *Store) InsertAccessToken(ctx context.Context, tokenHash []byte, userID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_access_tokens (token_hash, user_id, expires_at)
		VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("insert access token: %w", err)
	}
	return nil
}

// LookupAccessToken returns the user_id if the token is valid (exists,
// not expired, not revoked); empty string otherwise.
func (s *Store) LookupAccessToken(ctx context.Context, rawToken string) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id FROM codex_access_tokens
		WHERE token_hash = $1
		  AND expires_at > NOW()
		  AND revoked_at IS NULL`,
		HashToken(rawToken)).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup access token: %w", err)
	}
	return userID, nil
}

func (s *Store) InsertRefreshToken(ctx context.Context, tokenHash []byte, familyID, userID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_refresh_tokens (token_hash, family_id, user_id, expires_at)
		VALUES ($1, $2, $3, $4)`,
		tokenHash, familyID, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("insert refresh token: %w", err)
	}
	return nil
}

// RotateRefreshToken revokes the old refresh token and inserts a new
// one in the same family. Returns the user_id; errors if the old token
// is missing or already revoked.
func (s *Store) RotateRefreshToken(ctx context.Context, oldRaw string, newHash []byte, newExpiry time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	oldHash := HashToken(oldRaw)
	var userID, familyID string
	err = tx.QueryRowContext(ctx, `
		UPDATE codex_refresh_tokens
		SET revoked_at = NOW()
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > NOW()
		RETURNING user_id, family_id::text`,
		oldHash).Scan(&userID, &familyID)
	if err == sql.ErrNoRows {
		// Reuse detection (OAuth 2.1 §6.1): the token is either unknown
		// or already revoked. If it EXISTS but is revoked, assume the
		// family is compromised and burn every sibling token.
		var existingFamily string
		lookupErr := tx.QueryRowContext(ctx, `
			SELECT family_id::text FROM codex_refresh_tokens
			WHERE token_hash = $1`, oldHash).Scan(&existingFamily)
		if lookupErr == nil {
			if _, revokeErr := tx.ExecContext(ctx, `
				UPDATE codex_refresh_tokens
				SET revoked_at = NOW()
				WHERE family_id = $1::uuid AND revoked_at IS NULL`,
				existingFamily); revokeErr != nil {
				return "", fmt.Errorf("revoke family: %w", revokeErr)
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return "", fmt.Errorf("commit family revoke: %w", commitErr)
			}
		} else if !errors.Is(lookupErr, sql.ErrNoRows) {
			return "", fmt.Errorf("lookup revoked refresh: %w", lookupErr)
		}
		return "", fmt.Errorf("refresh token expired or revoked: %w", ErrRefreshTokenReuse)
	}
	if err != nil {
		return "", fmt.Errorf("revoke old refresh: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO codex_refresh_tokens (token_hash, family_id, user_id, expires_at)
		VALUES ($1, $2, $3, $4)`,
		newHash, familyID, userID, newExpiry); err != nil {
		return "", fmt.Errorf("insert new refresh: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return userID, nil
}

// ----- Agent Identity -----

type AgentIdentity struct {
	AgentRuntimeID string
	UserID         string
	PublicKey      []byte
	JWTSignedWith  string
	IssuedAt       time.Time
	ExpiresAt      time.Time
}

func (s *Store) InsertAgentIdentity(ctx context.Context, a AgentIdentity) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_agent_identities
			(agent_runtime_id, user_id, public_key, jwt_signed_with, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		a.AgentRuntimeID, a.UserID, a.PublicKey, a.JWTSignedWith, a.IssuedAt, a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert agent identity: %w", err)
	}
	return nil
}

func (s *Store) GetAgentIdentity(ctx context.Context, rid string) (*AgentIdentity, error) {
	var a AgentIdentity
	err := s.db.QueryRowContext(ctx, `
		SELECT agent_runtime_id, user_id, public_key, jwt_signed_with, issued_at, expires_at
		FROM codex_agent_identities
		WHERE agent_runtime_id = $1 AND revoked_at IS NULL AND expires_at > NOW()`,
		rid).Scan(&a.AgentRuntimeID, &a.UserID, &a.PublicKey, &a.JWTSignedWith, &a.IssuedAt, &a.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent identity: %w", err)
	}
	return &a, nil
}

// ----- Agent Tasks -----

type AgentTask struct {
	TaskID         string
	AgentRuntimeID string
	UserID         string
	IssuedAt       time.Time
	ExpiresAt      time.Time
}

func (s *Store) InsertAgentTask(ctx context.Context, taskID, rid, userID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_agent_tasks (task_id, agent_runtime_id, user_id, issued_at, expires_at)
		VALUES ($1, $2, $3, NOW(), $4)`,
		taskID, rid, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("insert agent task: %w", err)
	}
	return nil
}

func (s *Store) GetAgentTask(ctx context.Context, taskID string) (*AgentTask, error) {
	var t AgentTask
	err := s.db.QueryRowContext(ctx, `
		SELECT task_id, agent_runtime_id, user_id, issued_at, expires_at
		FROM codex_agent_tasks
		WHERE task_id = $1 AND expires_at > NOW()`,
		taskID).Scan(&t.TaskID, &t.AgentRuntimeID, &t.UserID, &t.IssuedAt, &t.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent task: %w", err)
	}
	return &t, nil
}

// ----- Device Codes -----

type DeviceCode struct {
	DeviceAuthID      string
	UserCode          string
	CodeChallenge     string
	CodeVerifier      string
	AuthorizationCode string
	Status            string
	UserID            string
	ExpiresAt         time.Time
	ApprovedAt        *time.Time
}

func (s *Store) InsertDeviceCode(ctx context.Context, dc DeviceCode) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO codex_device_codes
			(device_auth_id, user_code, code_challenge, code_verifier,
			 authorization_code, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		dc.DeviceAuthID, dc.UserCode, dc.CodeChallenge, dc.CodeVerifier,
		dc.AuthorizationCode, dc.Status, dc.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert device code: %w", err)
	}
	return nil
}

func (s *Store) GetDeviceCodeByUserCode(ctx context.Context, userCode string) (*DeviceCode, error) {
	var dc DeviceCode
	var uid sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT device_auth_id, user_code, code_challenge, code_verifier,
		       authorization_code, status, COALESCE(user_id, ''), expires_at
		FROM codex_device_codes
		WHERE user_code = $1 AND expires_at > NOW()`,
		userCode).Scan(&dc.DeviceAuthID, &dc.UserCode, &dc.CodeChallenge,
		&dc.CodeVerifier, &dc.AuthorizationCode, &dc.Status, &uid, &dc.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get device code: %w", err)
	}
	if uid.Valid {
		dc.UserID = uid.String
	}
	return &dc, nil
}

func (s *Store) ApproveDeviceCode(ctx context.Context, userCode, userID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE codex_device_codes
		SET status = 'approved', user_id = $2, approved_at = NOW()
		WHERE user_code = $1 AND status = 'pending' AND expires_at > NOW()`,
		userCode, userID)
	if err != nil {
		return fmt.Errorf("approve device code: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user_code not found or not pending")
	}
	return nil
}

// ExchangeDeviceCode atomically returns the row and marks it 'exchanged'.
// Only approved rows are returned; subsequent calls return nil.
func (s *Store) ExchangeDeviceCode(ctx context.Context, deviceAuthID, userCode string) (*DeviceCode, error) {
	var dc DeviceCode
	var uid sql.NullString
	err := s.db.QueryRowContext(ctx, `
		UPDATE codex_device_codes
		SET status = 'exchanged'
		WHERE device_auth_id = $1 AND user_code = $2 AND status = 'approved' AND expires_at > NOW()
		RETURNING device_auth_id, user_code, code_challenge, code_verifier,
		          authorization_code, status, COALESCE(user_id, ''), expires_at`,
		deviceAuthID, userCode).Scan(&dc.DeviceAuthID, &dc.UserCode, &dc.CodeChallenge,
		&dc.CodeVerifier, &dc.AuthorizationCode, &dc.Status, &uid, &dc.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("exchange device code: %w", err)
	}
	if uid.Valid {
		dc.UserID = uid.String
	}
	return &dc, nil
}
