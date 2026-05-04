CREATE TABLE IF NOT EXISTS agent_turns (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('queued','running','done','cancelled','failed')),
    user_event_id TEXT NOT NULL,
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
    im_channel_id TEXT,
    im_user_id    TEXT,
    user_message  TEXT NOT NULL,
    error_msg     TEXT,
    enqueued_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_agent_turns_pending ON agent_turns(session_id, enqueued_at)
    WHERE state IN ('queued','running');
CREATE INDEX IF NOT EXISTS idx_agent_turns_session ON agent_turns(session_id, enqueued_at DESC);

ALTER TABLE agent_session_events ADD COLUMN IF NOT EXISTS turn_id TEXT;
CREATE INDEX IF NOT EXISTS idx_agent_session_events_turn
    ON agent_session_events(turn_id) WHERE turn_id IS NOT NULL;
