-- Stateless CC sessions that originate from IM channels need to know
-- which channel to route replies through. Stores the IM channel ID
-- (workspace_im_channels.id) on the session at creation time.
ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS im_channel_id TEXT;
CREATE INDEX IF NOT EXISTS idx_agent_sessions_im_channel
    ON agent_sessions(im_channel_id) WHERE im_channel_id IS NOT NULL;
