CREATE TABLE IF NOT EXISTS workspace_quotas (
    workspace_id     TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    max_sandboxes    INTEGER,
    sandbox_cpu      TEXT,
    sandbox_memory   TEXT,
    idle_timeout     TEXT,
    max_total_cpu    TEXT,
    max_total_memory TEXT,
    drive_size       TEXT,
    updated_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
