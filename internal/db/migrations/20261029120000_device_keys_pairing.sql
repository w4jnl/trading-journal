-- migrate:up
-- Device keys and pairing for the phone app.
-- api_keys.kind: 'agent' (the default; an AI tool or script: read and
-- append, never overwrite) or 'device' (the trader's own phone: may edit
-- and delete journal entries, change settings, register for push).
-- api_keys.device_name: the phone's name for a device key.
-- pair_codes: one-time codes shown in Settings (as a QR) that a phone
-- exchanges for a device key within expires_at; used_at marks the exchange.
ALTER TABLE api_keys ADD COLUMN kind TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE api_keys ADD COLUMN device_name TEXT;

CREATE TABLE pair_codes (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_sha256 TEXT NOT NULL UNIQUE,
    device_hint TEXT NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  DATETIME NOT NULL,
    used_at     DATETIME
);
CREATE INDEX idx_pair_codes_user ON pair_codes(user_id);

-- migrate:down
DROP TABLE pair_codes;
ALTER TABLE api_keys DROP COLUMN device_name;
ALTER TABLE api_keys DROP COLUMN kind;
