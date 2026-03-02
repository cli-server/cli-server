-- Consolidated initial schema for agentserver v1.0.
-- This replaces all previous incremental migrations. Requires a fresh database.

-- Users
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT,
    picture       TEXT,
    role          TEXT NOT NULL DEFAULT 'user',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- User credentials (password hashes, separated from user identity)
CREATE TABLE user_credentials (
    user_id       TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Auth tokens
CREATE TABLE auth_tokens (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- OIDC identity mapping
CREATE TABLE oidc_identities (
    provider   TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, subject)
);
CREATE INDEX idx_oidc_identities_user_id ON oidc_identities(user_id);

-- Workspaces
CREATE TABLE workspaces (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    k8s_namespace TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Workspace volumes (PVCs / Docker volumes attached to a workspace)
CREATE TABLE workspace_volumes (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    pvc_name     TEXT NOT NULL,
    mount_path   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_workspace_volumes_workspace_id ON workspace_volumes(workspace_id);

-- Workspace members
CREATE TABLE workspace_members (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         TEXT NOT NULL DEFAULT 'developer',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX idx_workspace_members_user_id ON workspace_members(user_id);

-- Sandboxes
CREATE TABLE sandboxes (
    id                 TEXT PRIMARY KEY,
    workspace_id       TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    type               TEXT NOT NULL DEFAULT 'opencode',
    status             TEXT NOT NULL DEFAULT 'creating',
    is_local           BOOLEAN NOT NULL DEFAULT FALSE,
    sandbox_name       TEXT,
    pod_ip             TEXT,
    proxy_token        TEXT,
    opencode_token     TEXT,
    openclaw_token     TEXT,
    tunnel_token       TEXT,
    short_id           TEXT,
    cpu_millicores     INTEGER,
    memory_bytes       BIGINT,
    last_activity_at   TIMESTAMPTZ,
    last_heartbeat_at  TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    paused_at          TIMESTAMPTZ
);
CREATE INDEX idx_sandboxes_workspace_id ON sandboxes(workspace_id);
CREATE INDEX idx_sandboxes_status ON sandboxes(status);
CREATE INDEX idx_sandboxes_proxy_token ON sandboxes(proxy_token);
CREATE UNIQUE INDEX idx_sandboxes_short_id ON sandboxes (LOWER(short_id)) WHERE short_id IS NOT NULL;

-- Agent registration codes
CREATE TABLE agent_registration_codes (
    code         TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,
    used         BOOLEAN NOT NULL DEFAULT FALSE
);

-- System settings (key-value store for admin config)
CREATE TABLE system_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Per-user quota overrides
CREATE TABLE user_quotas (
    user_id        TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_workspaces INTEGER,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-workspace quota overrides
CREATE TABLE workspace_quotas (
    workspace_id         TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    max_sandboxes        INTEGER,
    max_sandbox_cpu      INTEGER,      -- millicores
    max_sandbox_memory   BIGINT,       -- bytes
    max_idle_timeout     INTEGER,      -- seconds
    max_total_cpu        INTEGER,      -- millicores
    max_total_memory     BIGINT,       -- bytes
    max_drive_size       BIGINT,       -- bytes
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
