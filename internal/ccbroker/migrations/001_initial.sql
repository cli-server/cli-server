CREATE TABLE IF NOT EXISTS agent_sessions (
    id            TEXT PRIMARY KEY,
    sandbox_id    TEXT,
    workspace_id  TEXT NOT NULL,
    title         TEXT,
    status        TEXT DEFAULT 'active',
    epoch         INTEGER DEFAULT 0,
    external_id   TEXT,
    source        TEXT DEFAULT 'agent',
    tags          TEXT[],
    created_at    TIMESTAMPTZ DEFAULT NOW(),
    updated_at    TIMESTAMPTZ DEFAULT NOW(),
    archived_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS agent_session_events (
    id            BIGSERIAL PRIMARY KEY,
    session_id    TEXT NOT NULL,
    event_id      TEXT NOT NULL UNIQUE,
    event_type    TEXT DEFAULT 'client_event',
    source        TEXT DEFAULT 'client',
    epoch         INTEGER,
    payload       JSONB NOT NULL,
    ephemeral     BOOLEAN DEFAULT FALSE,
    created_at    TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ase_session_id ON agent_session_events(session_id, id);

CREATE TABLE IF NOT EXISTS agent_session_workers (
    session_id              TEXT NOT NULL,
    epoch                   INTEGER NOT NULL,
    state                   TEXT DEFAULT 'idle',
    external_metadata       JSONB,
    requires_action_details JSONB,
    last_heartbeat_at       TIMESTAMPTZ,
    registered_at           TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (session_id, epoch)
);

CREATE TABLE IF NOT EXISTS agent_session_internal_events (
    id            BIGSERIAL PRIMARY KEY,
    session_id    TEXT NOT NULL,
    event_type    TEXT,
    payload       JSONB,
    is_compaction BOOLEAN DEFAULT FALSE,
    agent_id      TEXT,
    created_at    TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_asie_session ON agent_session_internal_events(session_id, id);
