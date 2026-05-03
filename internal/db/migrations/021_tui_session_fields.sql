-- 021_tui_session_fields.sql
-- Adds session fields needed by TUI client (channel routing, model preference,
-- permission mode, preferred executor, responder claim, active turn CAS).

ALTER TABLE agent_sessions
  ADD COLUMN IF NOT EXISTS channel_type           TEXT NOT NULL DEFAULT 'im',
  ADD COLUMN IF NOT EXISTS creator_user_id        TEXT,
  ADD COLUMN IF NOT EXISTS preferred_model        TEXT,
  ADD COLUMN IF NOT EXISTS permission_mode        TEXT NOT NULL DEFAULT 'bypass',
  ADD COLUMN IF NOT EXISTS preferred_executor_id  TEXT,
  ADD COLUMN IF NOT EXISTS permission_responder   TEXT,
  ADD COLUMN IF NOT EXISTS responder_attached_at  TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS active_turn_id         TEXT;

UPDATE agent_sessions SET creator_user_id = 'unknown' WHERE creator_user_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_agent_sessions_channel_external
  ON agent_sessions (workspace_id, channel_type, external_id);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_responder
  ON agent_sessions (permission_responder) WHERE permission_responder IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_sessions_active_turn
  ON agent_sessions (active_turn_id) WHERE active_turn_id IS NOT NULL;
