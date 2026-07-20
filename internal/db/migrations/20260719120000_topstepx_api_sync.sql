-- migrate:up
-- TopstepX API sync: per-user API credentials (apiKey stored AES-256-GCM
-- encrypted with a key from config.yaml; username is not secret) and a
-- per-account flag marking that the initial month-by-month history walk-back
-- has completed, so never-traded accounts are not re-walked on every sync.
-- Incremental sync state is intentionally NOT stored: the next window is
-- derived from MAX(executions.exec_time) per account, and last-sync display
-- comes from sync_log (sync_type = 'topstepx_api').
CREATE TABLE topstepx_credentials (
    user_id          INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    username         TEXT NOT NULL,
    api_key_enc      BLOB NOT NULL,
    api_key_nonce    BLOB NOT NULL,
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_verified_at DATETIME
);

ALTER TABLE accounts ADD COLUMN api_backfill_done INTEGER NOT NULL DEFAULT 0;

-- migrate:down
ALTER TABLE accounts DROP COLUMN api_backfill_done;
DROP TABLE topstepx_credentials;
