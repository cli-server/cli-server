-- Unified IM binding table (replaces sandbox_weixin_bindings for new code)
CREATE TABLE IF NOT EXISTS sandbox_im_bindings (
    id              SERIAL PRIMARY KEY,
    sandbox_id      TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,
    bot_id          TEXT NOT NULL,
    user_id         TEXT NOT NULL DEFAULT '',
    bot_token       TEXT,
    base_url        TEXT,
    cursor          TEXT,
    last_poll_at    TIMESTAMPTZ,
    bound_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_im_bindings_unique ON sandbox_im_bindings(sandbox_id, provider, bot_id);
CREATE INDEX idx_im_bindings_sandbox ON sandbox_im_bindings(sandbox_id);
CREATE INDEX idx_im_bindings_provider_bot ON sandbox_im_bindings(provider, bot_id);

-- Provider-specific per-user metadata (replaces weixin_context_tokens for new code)
CREATE TABLE IF NOT EXISTS im_provider_meta (
    sandbox_id  TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,
    bot_id      TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    meta_key    TEXT NOT NULL,
    meta_value  TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (sandbox_id, provider, bot_id, user_id, meta_key)
);

-- Migrate existing WeChat bindings data
INSERT INTO sandbox_im_bindings
    (sandbox_id, provider, bot_id, user_id, bot_token, base_url, cursor, last_poll_at, bound_at)
SELECT
    sandbox_id, 'weixin', bot_id, user_id, bot_token, ilink_base_url, get_updates_buf, last_poll_at, bound_at
FROM sandbox_weixin_bindings
ON CONFLICT DO NOTHING;

-- Migrate existing WeChat context tokens
INSERT INTO im_provider_meta
    (sandbox_id, provider, bot_id, user_id, meta_key, meta_value, updated_at)
SELECT
    sandbox_id, 'weixin', bot_id, user_id, 'context_token', context_token, updated_at
FROM weixin_context_tokens
ON CONFLICT DO NOTHING;

-- Old tables (sandbox_weixin_bindings, weixin_context_tokens) are NOT dropped.
-- They are retained for rollback safety. A future migration will drop them.
