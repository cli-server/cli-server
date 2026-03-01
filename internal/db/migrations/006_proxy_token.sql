-- Add proxy_token column for per-session Anthropic API proxy authentication.
-- Sandbox containers use this token as their "API key" when calling the agentserver proxy,
-- which then injects the real Anthropic API key before forwarding to the Anthropic API.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS proxy_token TEXT;
CREATE INDEX IF NOT EXISTS idx_sessions_proxy_token ON sessions(proxy_token);
