-- Add client-info columns captured on each inbound /inbound/{exe_id} connect.
-- These power the Connectors UI's IP / OS / codex-version / online-status columns
-- and replace the old "last_seen_at < 90s" frontend heuristic.
--
-- last_seen_at is kept for back-compat but no longer touched on disconnect; the
-- new connected_at / disconnected_at pair is the source of truth for "currently
-- online" + "when did this last fall offline".
ALTER TABLE executors
    ADD COLUMN IF NOT EXISTS client_ip       TEXT,
    ADD COLUMN IF NOT EXISTS client_ua       TEXT,
    ADD COLUMN IF NOT EXISTS codex_version   TEXT,
    ADD COLUMN IF NOT EXISTS os              TEXT,
    ADD COLUMN IF NOT EXISTS connected_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS disconnected_at TIMESTAMPTZ;
