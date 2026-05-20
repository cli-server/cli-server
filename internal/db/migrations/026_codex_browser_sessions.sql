-- Per-connection record for each `codex --remote` ws session against
-- codex-app-gateway. One token can be used by many machines concurrently;
-- each ws open inserts a row, ws close stamps disconnected_at, and the
-- workspace UI's Browsers panel joins these to render IP / OS / codex
-- version / online status per token.
CREATE TABLE IF NOT EXISTS codex_browser_sessions (
    id              TEXT PRIMARY KEY,
    token_id        TEXT NOT NULL REFERENCES codex_remote_tokens(id) ON DELETE CASCADE,
    client_ip       TEXT,
    client_ua       TEXT,
    codex_version   TEXT,
    os              TEXT,
    connected_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disconnected_at TIMESTAMPTZ
);

-- Hot index: list-open-sessions-for-token. Partial so it doesn't bloat
-- with the long historical tail of disconnected sessions.
CREATE INDEX IF NOT EXISTS idx_codex_browser_sessions_token_open
    ON codex_browser_sessions(token_id)
    WHERE disconnected_at IS NULL;

-- Lookup index for the "latest session for token" join (online or recent).
CREATE INDEX IF NOT EXISTS idx_codex_browser_sessions_token_connected
    ON codex_browser_sessions(token_id, connected_at DESC);
