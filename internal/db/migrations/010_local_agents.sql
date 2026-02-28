ALTER TABLE sandboxes ADD COLUMN is_local BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE sandboxes ADD COLUMN tunnel_token TEXT;
ALTER TABLE sandboxes ADD COLUMN last_heartbeat_at TIMESTAMPTZ;

CREATE TABLE agent_registration_codes (
    code       TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    used       BOOLEAN NOT NULL DEFAULT FALSE
);
