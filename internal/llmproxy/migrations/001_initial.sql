-- LLM Proxy: initial schema
-- Trace table: one trace groups multiple API requests (e.g. a Claude Code session)
CREATE TABLE traces (
    id           TEXT PRIMARY KEY,
    sandbox_id   TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT 'unknown',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_traces_sandbox_id ON traces(sandbox_id);
CREATE INDEX idx_traces_workspace_id ON traces(workspace_id);
CREATE INDEX idx_traces_created_at ON traces(created_at);

-- Usage table: each row is a single LLM API request with token counts
CREATE TABLE usage (
    id                          TEXT PRIMARY KEY,
    trace_id                    TEXT REFERENCES traces(id) ON DELETE SET NULL,
    sandbox_id                  TEXT NOT NULL,
    workspace_id                TEXT NOT NULL,
    provider                    TEXT NOT NULL,
    model                       TEXT NOT NULL,
    message_id                  TEXT,
    input_tokens                BIGINT NOT NULL DEFAULT 0,
    output_tokens               BIGINT NOT NULL DEFAULT 0,
    cache_creation_input_tokens BIGINT NOT NULL DEFAULT 0,
    cache_read_input_tokens     BIGINT NOT NULL DEFAULT 0,
    streaming                   BOOLEAN NOT NULL DEFAULT FALSE,
    duration_ms                 BIGINT NOT NULL DEFAULT 0,
    ttft_ms                     BIGINT NOT NULL DEFAULT 0,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_usage_trace_id ON usage(trace_id);
CREATE INDEX idx_usage_sandbox_id ON usage(sandbox_id);
CREATE INDEX idx_usage_workspace_id ON usage(workspace_id);
CREATE INDEX idx_usage_created_at ON usage(created_at);
CREATE INDEX idx_usage_provider_model ON usage(provider, model);
