-- 025_codex_auth.sql
-- Self-hosted codex auth: PKCE + device flow + Agent Identity JWT minting.
-- Spec: docs/superpowers/specs/2026-05-20-codex-0.132-auth.md

-- ChatGPT mode --------------------------------------------------------
CREATE TABLE codex_pkce_requests (
    code            TEXT PRIMARY KEY,
    code_challenge  TEXT NOT NULL,
    state           TEXT NOT NULL,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_codex_pkce_requests_expires_at ON codex_pkce_requests (expires_at);

CREATE TABLE codex_access_tokens (
    token_hash      BYTEA PRIMARY KEY,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ
);
CREATE INDEX idx_codex_access_tokens_user_id  ON codex_access_tokens (user_id);
CREATE INDEX idx_codex_access_tokens_expires  ON codex_access_tokens (expires_at);

CREATE TABLE codex_refresh_tokens (
    token_hash      BYTEA PRIMARY KEY,
    family_id       UUID NOT NULL,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ
);
CREATE INDEX idx_codex_refresh_tokens_family ON codex_refresh_tokens (family_id);

CREATE TABLE codex_device_codes (
    device_auth_id      TEXT PRIMARY KEY,
    user_code           TEXT NOT NULL UNIQUE,
    code_challenge      TEXT NOT NULL,
    code_verifier       TEXT NOT NULL,
    authorization_code  TEXT NOT NULL,
    status              TEXT NOT NULL,  -- pending|approved|exchanged|denied
    user_id             UUID REFERENCES users(id) ON DELETE CASCADE,
    expires_at          TIMESTAMPTZ NOT NULL,
    approved_at         TIMESTAMPTZ
);
CREATE INDEX idx_codex_device_codes_user_code ON codex_device_codes (user_code);
CREATE INDEX idx_codex_device_codes_expires   ON codex_device_codes (expires_at);

-- Agent Identity mode -------------------------------------------------
CREATE TABLE codex_jwks_keys (
    kid             TEXT PRIMARY KEY,
    public_n        TEXT NOT NULL,
    public_e        TEXT NOT NULL,
    private_pkcs8   BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    active          BOOL NOT NULL
);
CREATE UNIQUE INDEX uniq_codex_jwks_keys_one_active
    ON codex_jwks_keys (active)
    WHERE active;

CREATE TABLE codex_agent_identities (
    agent_runtime_id  TEXT PRIMARY KEY,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    public_key        BYTEA NOT NULL,
    jwt_signed_with   TEXT NOT NULL REFERENCES codex_jwks_keys(kid),
    issued_at         TIMESTAMPTZ NOT NULL,
    expires_at        TIMESTAMPTZ NOT NULL,
    revoked_at        TIMESTAMPTZ
);

CREATE TABLE codex_agent_tasks (
    task_id           TEXT PRIMARY KEY,
    agent_runtime_id  TEXT NOT NULL REFERENCES codex_agent_identities(agent_runtime_id) ON DELETE CASCADE,
    user_id           UUID NOT NULL,
    issued_at         TIMESTAMPTZ NOT NULL,
    expires_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_codex_agent_tasks_expires ON codex_agent_tasks (expires_at);
