-- Add opencode_password column for per-session opencode server authentication.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS opencode_password TEXT;
