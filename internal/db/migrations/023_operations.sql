-- v1 operation log. Written by codex-app-gateway via POST /internal/operations
-- whenever an mcpServer/tool/call request/response pair completes.
--
-- Columns marked v1.5 are populated only when env-mcp itself emits the record
-- (LLM-initiated calls) or when the IPython kernel attribution metadata is
-- wired up. v1 leaves them NULL.

CREATE TABLE operations (
  id              UUID PRIMARY KEY,
  workspace_id    TEXT NOT NULL,
  user_id         TEXT,
  source          TEXT NOT NULL,
  thread_id       TEXT,
  request_id      TEXT,

  env_id          TEXT NOT NULL,
  tool            TEXT NOT NULL,
  arguments       JSONB,
  arguments_meta  JSONB,

  is_error        BOOLEAN NOT NULL,
  result_summary  TEXT,
  result_meta     JSONB,

  started_at      TIMESTAMPTZ NOT NULL,
  completed_at    TIMESTAMPTZ NOT NULL,
  duration_ms     INTEGER NOT NULL,

  -- v1.5
  notebook_path   TEXT,
  cell_id         TEXT
);

CREATE INDEX ops_ws_time   ON operations (workspace_id, started_at DESC);
CREATE INDEX ops_ws_env    ON operations (workspace_id, env_id, started_at DESC);
CREATE INDEX ops_ws_source ON operations (workspace_id, source, started_at DESC);
