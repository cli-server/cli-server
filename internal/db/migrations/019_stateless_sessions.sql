-- Make sandbox_id nullable (stateless CC sessions have no sandbox)
ALTER TABLE agent_sessions ALTER COLUMN sandbox_id DROP NOT NULL;

-- Add external_id for IM session resolution (chat_jid → session mapping)
ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS external_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_sessions_external_id
    ON agent_sessions(workspace_id, external_id) WHERE external_id IS NOT NULL;

-- Add source field to identify session origin
ALTER TABLE agent_sessions ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'agent';
