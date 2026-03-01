CREATE TABLE system_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE user_quotas (
    user_id                     TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_workspaces              INTEGER,
    max_sandboxes_per_workspace INTEGER,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
