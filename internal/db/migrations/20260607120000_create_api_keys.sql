-- migrate:up

-- API keys: per-user bearer credentials for the read/append HTTP API. The
-- plaintext key is shown to the user once at creation and never stored; only
-- sha256(key) is persisted, in a UNIQUE column for O(1) lookup (the key is
-- high-entropy, so a fast hash is correct here - bcrypt could not be looked up
-- without scanning every row). key_prefix is a short, non-secret display string
-- (e.g. "tj_Ab12Cd3") shown in the settings UI. A key is active while
-- revoked_at IS NULL.
CREATE TABLE api_keys (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    key_prefix   TEXT NOT NULL,
    key_sha256   TEXT NOT NULL UNIQUE,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at DATETIME,
    revoked_at   DATETIME
);
CREATE INDEX idx_api_keys_user ON api_keys(user_id);

-- migrate:down
DROP INDEX idx_api_keys_user;
DROP TABLE api_keys;
