-- migrate:up
-- Per-user AI provider for the in-app reviews: the user's own key for a hosted
-- provider (or, for admins, any OpenAI-compatible URL), used instead of the
-- server-wide ai default. No row = the server default. api_key_enc/nonce are the
-- AES-256-GCM ciphertext + nonce from internal/secretbox (the plaintext key never
-- touches the database); both NULL for a keyless custom endpoint. base_url is
-- set for the custom provider only - hosted providers have fixed endpoints.
-- key_hint keeps the key's last 4 characters so Settings can say which key is
-- stored without decrypting it. A row is only written after a live test call
-- succeeded, so last_verified_at is set on every save.
CREATE TABLE user_ai_settings (
    user_id          INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    provider         TEXT NOT NULL,
    model            TEXT NOT NULL,
    base_url         TEXT NOT NULL DEFAULT '',
    api_key_enc      BLOB,
    api_key_nonce    BLOB,
    key_hint         TEXT NOT NULL DEFAULT '',
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_verified_at DATETIME
);

-- migrate:down
DROP TABLE user_ai_settings;
