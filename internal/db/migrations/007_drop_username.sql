-- Username is no longer required; email is the sole user identifier.
ALTER TABLE users ALTER COLUMN username DROP NOT NULL;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_username_key;
