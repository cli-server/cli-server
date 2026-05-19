-- 024_codex_thread_id.sql
-- Add codex_thread_id to agent_sessions so the codex routing path
-- can persist the codex Thread id per (workspace_id, external_id)
-- conversation. Coexists with cc_thread_id (used by stateless_cc).

ALTER TABLE agent_sessions ADD COLUMN codex_thread_id TEXT;
