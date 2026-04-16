CREATE TABLE IF NOT EXISTS executors (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL CHECK (type IN ('local_agent', 'sandbox')),
    status          TEXT NOT NULL DEFAULT 'online',
    tunnel_token_hash TEXT,
    registry_token_hash TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_executors_workspace ON executors(workspace_id);
CREATE INDEX idx_executors_status ON executors(status);

CREATE TABLE IF NOT EXISTS executor_capabilities (
    executor_id     TEXT PRIMARY KEY REFERENCES executors(id) ON DELETE CASCADE,
    tools           JSONB NOT NULL DEFAULT '[]',
    environment     JSONB NOT NULL DEFAULT '{}',
    resources       JSONB NOT NULL DEFAULT '{}',
    description     TEXT NOT NULL DEFAULT '',
    working_dir     TEXT NOT NULL DEFAULT '',
    probed_at       TIMESTAMPTZ,
    user_declared   JSONB
);

CREATE TABLE IF NOT EXISTS executor_heartbeats (
    executor_id     TEXT PRIMARY KEY REFERENCES executors(id) ON DELETE CASCADE,
    last_seen       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    system_info     JSONB NOT NULL DEFAULT '{}'
);
