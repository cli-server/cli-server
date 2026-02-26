-- Add email column to users (nullable, unique when non-null).
ALTER TABLE users ADD COLUMN email TEXT;
CREATE UNIQUE INDEX idx_users_email ON users(email) WHERE email IS NOT NULL;

-- Make password_hash nullable so OIDC-only users can exist.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

-- OIDC identity mapping table.
CREATE TABLE oidc_identities (
    provider   TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, subject)
);
CREATE INDEX idx_oidc_identities_user_id ON oidc_identities(user_id);
