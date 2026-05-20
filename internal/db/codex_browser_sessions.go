package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CodexBrowserSession is a single live (or historical) `codex --remote` ws
// connection. The row is inserted by CXG on ws accept and stamped with
// disconnected_at on ws close. Multiple concurrent rows per token_id are
// supported — a token can be used from N machines simultaneously.
type CodexBrowserSession struct {
	ID             string
	TokenID        string
	ClientIP       string
	ClientUA       string
	CodexVersion   string
	OS             string
	ConnectedAt    time.Time
	DisconnectedAt *time.Time
}

// CreateCodexBrowserSession inserts a new session row. The caller supplies
// the id (cryptographic random, namespaced separately from token ids so a
// leaked session id can't be confused with a token).
func (db *DB) CreateCodexBrowserSession(ctx context.Context, s CodexBrowserSession) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO codex_browser_sessions
		    (id, token_id, client_ip, client_ua, codex_version, os, connected_at)
		VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), NULLIF($6,''), NOW())`,
		s.ID, s.TokenID, s.ClientIP, s.ClientUA, s.CodexVersion, s.OS)
	if err != nil {
		return fmt.Errorf("insert codex_browser_sessions: %w", err)
	}
	return nil
}

// CloseCodexBrowserSession stamps disconnected_at. Idempotent: a missing or
// already-closed row is a no-op.
func (db *DB) CloseCodexBrowserSession(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE codex_browser_sessions
		   SET disconnected_at = NOW()
		 WHERE id = $1 AND disconnected_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("close codex_browser_sessions: %w", err)
	}
	return nil
}

// SweepStaleCodexBrowserSessions marks any session connected before olderThan
// and not yet closed as disconnected. Called from CXG startup so rows from
// a crashed previous CXG pod don't leak forever as "online".
func (db *DB) SweepStaleCodexBrowserSessions(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE codex_browser_sessions
		   SET disconnected_at = NOW()
		 WHERE disconnected_at IS NULL AND connected_at < $1`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("sweep codex_browser_sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// LatestCodexBrowserSession returns the most recent session for a token (open
// if any, otherwise the latest historical one), or (nil, nil) if the token
// has never been used.
func (db *DB) LatestCodexBrowserSession(ctx context.Context, tokenID string) (*CodexBrowserSession, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, token_id,
		       COALESCE(client_ip, ''), COALESCE(client_ua, ''),
		       COALESCE(codex_version, ''), COALESCE(os, ''),
		       connected_at, disconnected_at
		  FROM codex_browser_sessions
		 WHERE token_id = $1
		 ORDER BY (disconnected_at IS NULL) DESC, connected_at DESC
		 LIMIT 1`, tokenID)
	var s CodexBrowserSession
	var disconnected sql.NullTime
	if err := row.Scan(&s.ID, &s.TokenID, &s.ClientIP, &s.ClientUA,
		&s.CodexVersion, &s.OS, &s.ConnectedAt, &disconnected); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan codex_browser_sessions: %w", err)
	}
	if disconnected.Valid {
		t := disconnected.Time
		s.DisconnectedAt = &t
	}
	return &s, nil
}

// CountOpenCodexBrowserSessions returns the number of open (disconnected_at IS
// NULL) sessions for a token. Used by the Browsers list endpoint to set
// is_online without iterating all rows.
func (db *DB) CountOpenCodexBrowserSessions(ctx context.Context, tokenID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM codex_browser_sessions WHERE token_id = $1 AND disconnected_at IS NULL`,
		tokenID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count codex_browser_sessions: %w", err)
	}
	return n, nil
}
