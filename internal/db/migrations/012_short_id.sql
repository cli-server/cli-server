ALTER TABLE sandboxes ADD COLUMN short_id TEXT;
CREATE UNIQUE INDEX idx_sandboxes_short_id ON sandboxes (short_id) WHERE short_id IS NOT NULL;
