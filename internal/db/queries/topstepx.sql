-- name: UpsertTopstepxCredentials :exec
-- Save (or replace) a user's TopstepX API credentials. api_key_enc/nonce are
-- the AES-256-GCM ciphertext + nonce produced by topstepx.SecretBox; the
-- plaintext key never touches the database. Saving implies the credentials
-- were just verified via loginKey, so last_verified_at is refreshed too.
-- Any stored session token is cleared so the new credentials start with a
-- fresh login.
INSERT INTO topstepx_credentials (user_id, username, api_key_enc, api_key_nonce, last_verified_at)
VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(user_id) DO UPDATE SET
    username = excluded.username,
    api_key_enc = excluded.api_key_enc,
    api_key_nonce = excluded.api_key_nonce,
    updated_at = CURRENT_TIMESTAMP,
    last_verified_at = CURRENT_TIMESTAMP,
    token_enc = NULL,
    token_nonce = NULL,
    token_expires_at = NULL;

-- name: GetTopstepxCredentials :one
SELECT * FROM topstepx_credentials WHERE user_id = ?;

-- name: DeleteTopstepxCredentials :exec
DELETE FROM topstepx_credentials WHERE user_id = ?;

-- name: TouchTopstepxVerified :exec
UPDATE topstepx_credentials SET last_verified_at = CURRENT_TIMESTAMP WHERE user_id = ?;

-- name: SetTopstepxToken :exec
-- Persist the (encrypted) session token + its JWT expiry so restarts reuse
-- it and the background maintainer can renew it before it lapses.
UPDATE topstepx_credentials
SET token_enc = ?, token_nonce = ?, token_expires_at = ?
WHERE user_id = ?;

-- name: ClearTopstepxToken :exec
-- Drop a rejected/stale token; the next refresh starts from loginKey.
UPDATE topstepx_credentials
SET token_enc = NULL, token_nonce = NULL, token_expires_at = NULL
WHERE user_id = ?;

-- name: ListTopstepxSyncUsers :many
-- Users the hourly scheduler fans out to: credentials stored and user active.
SELECT c.user_id FROM topstepx_credentials c
JOIN users u ON u.id = c.user_id
WHERE u.is_active = 1;
