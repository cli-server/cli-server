-- Chat messages and streaming events
CREATE TABLE messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role VARCHAR(16) NOT NULL,  -- 'user' or 'assistant'
    content_text TEXT NOT NULL DEFAULT '',
    content_render JSONB NOT NULL DEFAULT '{"events":[]}',
    last_seq BIGINT NOT NULL DEFAULT 0,
    active_stream_id UUID,
    stream_status VARCHAR(32) NOT NULL DEFAULT 'completed',
    model_id VARCHAR(128),
    total_cost_usd FLOAT DEFAULT 0.0,
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX idx_messages_session ON messages(session_id, created_at);

CREATE TABLE message_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    stream_id UUID NOT NULL,
    seq BIGINT NOT NULL,
    event_type VARCHAR(64) NOT NULL,
    render_payload JSONB NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE UNIQUE INDEX idx_me_stream_seq ON message_events(stream_id, seq);
CREATE INDEX idx_me_session_seq ON message_events(session_id, seq);
