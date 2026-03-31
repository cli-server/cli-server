-- Add generic metadata column for sandbox-specific configuration.
-- Stores JSON key-value pairs (e.g. assistant_name for nanoclaw).
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
