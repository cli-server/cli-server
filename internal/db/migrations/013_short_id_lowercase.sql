-- Normalise existing short_ids to lowercase (subdomains are case-insensitive).
UPDATE sandboxes SET short_id = LOWER(short_id) WHERE short_id IS NOT NULL;

-- Replace the old case-sensitive index with a case-insensitive one.
DROP INDEX IF EXISTS idx_sandboxes_short_id;
CREATE UNIQUE INDEX idx_sandboxes_short_id ON sandboxes (LOWER(short_id)) WHERE short_id IS NOT NULL;
