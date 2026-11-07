-- name: GetUserAISettings :one
SELECT * FROM user_ai_settings WHERE user_id = ?;

-- name: UserHasAISettings :one
-- Cheap per-page check behind the AI buttons when there is no server default.
SELECT COUNT(*) FROM user_ai_settings WHERE user_id = ?;

-- name: UpsertUserAISettings :exec
-- Save (or replace) the user's provider, model, effort and key. Saving implies
-- a live test call just succeeded, so last_verified_at is refreshed too.
INSERT INTO user_ai_settings (user_id, provider, model, base_url, effort, api_key_enc, api_key_nonce, key_hint, last_verified_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(user_id) DO UPDATE SET
    provider = excluded.provider,
    model = excluded.model,
    base_url = excluded.base_url,
    effort = excluded.effort,
    api_key_enc = excluded.api_key_enc,
    api_key_nonce = excluded.api_key_nonce,
    key_hint = excluded.key_hint,
    updated_at = CURRENT_TIMESTAMP,
    last_verified_at = CURRENT_TIMESTAMP;

-- name: TouchUserAISettingsVerified :exec
UPDATE user_ai_settings SET last_verified_at = CURRENT_TIMESTAMP WHERE user_id = ?;

-- name: DeleteUserAISettings :exec
DELETE FROM user_ai_settings WHERE user_id = ?;

-- name: GetUserAITier :one
SELECT * FROM user_ai_tiers WHERE user_id = ? AND tier = ?;

-- name: ListUserAITiers :many
SELECT * FROM user_ai_tiers WHERE user_id = ? ORDER BY tier;

-- name: UpsertUserAITier :exec
INSERT INTO user_ai_tiers (user_id, tier, model, effort)
VALUES (?, ?, ?, ?)
ON CONFLICT(user_id, tier) DO UPDATE SET
    model = excluded.model,
    effort = excluded.effort,
    updated_at = CURRENT_TIMESTAMP;

-- name: DeleteUserAITier :exec
DELETE FROM user_ai_tiers WHERE user_id = ? AND tier = ?;

-- name: DeleteUserAITiers :exec
-- The tier models belong to a provider: a new provider or no provider drops them.
DELETE FROM user_ai_tiers WHERE user_id = ?;
