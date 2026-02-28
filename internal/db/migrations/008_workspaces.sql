-- New tables
CREATE TABLE workspaces (
    id TEXT PRIMARY KEY, name TEXT NOT NULL,
    disk_pvc_name TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE workspace_members (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL DEFAULT 'developer',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX idx_workspace_members_user_id ON workspace_members(user_id);

CREATE TABLE sandboxes (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name TEXT NOT NULL, type TEXT NOT NULL DEFAULT 'opencode',
    status TEXT NOT NULL DEFAULT 'creating',
    sandbox_name TEXT, pod_ip TEXT,
    opencode_password TEXT, proxy_token TEXT,
    last_activity_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), paused_at TIMESTAMPTZ
);
CREATE INDEX idx_sandboxes_workspace_id ON sandboxes(workspace_id);
CREATE INDEX idx_sandboxes_status ON sandboxes(status);
CREATE INDEX idx_sandboxes_proxy_token ON sandboxes(proxy_token);

-- Drop old tables
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS user_drives;
