-- Workspace-level IM channels (replaces per-sandbox sandbox_im_bindings)
CREATE TABLE IF NOT EXISTS workspace_im_channels (
    id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    bot_id TEXT NOT NULL,
    user_id TEXT NOT NULL DEFAULT '',
    bot_token TEXT,
    base_url TEXT,
    cursor TEXT DEFAULT '',
    bound_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(workspace_id, provider, bot_id)
);
CREATE INDEX IF NOT EXISTS idx_workspace_im_channels_workspace ON workspace_im_channels(workspace_id);

-- Per-user metadata for workspace IM channels
CREATE TABLE IF NOT EXISTS workspace_im_channel_meta (
    channel_id TEXT NOT NULL REFERENCES workspace_im_channels(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    meta_key TEXT NOT NULL,
    meta_value TEXT NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (channel_id, user_id, meta_key)
);

-- Link sandboxes to workspace IM channels
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS im_channel_id TEXT REFERENCES workspace_im_channels(id) ON DELETE SET NULL;

-- Migrate existing sandbox_im_bindings into workspace_im_channels
INSERT INTO workspace_im_channels (workspace_id, provider, bot_id, user_id, bot_token, base_url, cursor, bound_at)
SELECT s.workspace_id, b.provider, b.bot_id, COALESCE(b.user_id, ''), b.bot_token, b.base_url, b.cursor, b.bound_at
FROM sandbox_im_bindings b
JOIN sandboxes s ON s.id = b.sandbox_id
WHERE b.bot_token IS NOT NULL
ON CONFLICT (workspace_id, provider, bot_id) DO NOTHING;

-- Set im_channel_id on sandboxes that had bindings
UPDATE sandboxes s SET im_channel_id = c.id
FROM sandbox_im_bindings b
JOIN workspace_im_channels c ON c.provider = b.provider AND c.bot_id = b.bot_id
JOIN sandboxes s2 ON s2.id = b.sandbox_id AND s2.workspace_id = c.workspace_id
WHERE s.id = b.sandbox_id AND b.bot_token IS NOT NULL;

-- Migrate im_provider_meta data into workspace_im_channel_meta
INSERT INTO workspace_im_channel_meta (channel_id, user_id, meta_key, meta_value, updated_at)
SELECT c.id, m.user_id, m.meta_key, m.meta_value, m.updated_at
FROM im_provider_meta m
JOIN sandbox_im_bindings b ON b.sandbox_id = m.sandbox_id AND b.provider = m.provider AND b.bot_id = m.bot_id
JOIN sandboxes s ON s.id = b.sandbox_id
JOIN workspace_im_channels c ON c.workspace_id = s.workspace_id AND c.provider = b.provider AND c.bot_id = b.bot_id
ON CONFLICT (channel_id, user_id, meta_key) DO NOTHING;

-- Old tables (sandbox_im_bindings, im_provider_meta) are NOT dropped.
-- They are retained for rollback safety. A future migration will drop them.
