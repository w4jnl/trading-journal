-- name: GetUserAISettings :one
SELECT * FROM user_ai_settings WHERE user_id = ?;

-- name: UserHasAISettings :one
-- Cheap per-page check behind the AI buttons when there is no server default.
SELECT COUNT(*) FROM user_ai_settings WHERE user_id = ?;

-- name: UpsertUserAISettings :exec
-- Save (or replace) the user's provider, model and key. Saving implies a live
-- test call just succeeded, so last_verified_at is refreshed too.
INSERT INTO user_ai_settings (user_id, provider, model, base_url, api_key_enc, api_key_nonce, key_hint, last_verified_at)
VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(user_id) DO UPDATE SET
    provider = excluded.provider,
    model = excluded.model,
    base_url = excluded.base_url,
    api_key_enc = excluded.api_key_enc,
    api_key_nonce = excluded.api_key_nonce,
    key_hint = excluded.key_hint,
    updated_at = CURRENT_TIMESTAMP,
    last_verified_at = CURRENT_TIMESTAMP;

-- name: TouchUserAISettingsVerified :exec
UPDATE user_ai_settings SET last_verified_at = CURRENT_TIMESTAMP WHERE user_id = ?;

-- name: DeleteUserAISettings :exec
DELETE FROM user_ai_settings WHERE user_id = ?;
