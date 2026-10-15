-- name: CreateAPIKey :one
-- kind is 'agent' or 'device'; device_name names the phone of a device key.
INSERT INTO api_keys (user_id, name, key_prefix, key_sha256, kind, device_name)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListAPIKeysForUser :many
-- Newest first; includes revoked keys so the settings UI can show their state.
SELECT id, user_id, name, key_prefix, kind, device_name, created_at, last_used_at, revoked_at
FROM api_keys
WHERE user_id = ?
ORDER BY created_at DESC, id DESC;

-- name: GetAPIKeyBySHA256 :one
-- Auth lookup. Joins users (COALESCE so display_name/email come back as plain
-- strings, mirroring GetSession) so the middleware builds the context user in
-- one round-trip. Only non-revoked keys resolve; is_active lets the middleware
-- also reject deactivated users. NOTE: if an expires_at column is added later,
-- AND it here: AND (k.expires_at IS NULL OR k.expires_at > CURRENT_TIMESTAMP).
SELECT k.id         AS key_id,
       k.user_id    AS user_id,
       k.name       AS key_name,
       k.key_prefix AS key_prefix,
       k.kind       AS key_kind,
       k.created_at AS key_created_at,
       u.username,
       COALESCE(u.display_name, u.username) AS display_name,
       COALESCE(u.email, '')                AS email,
       u.is_admin,
       u.is_active
FROM api_keys k
JOIN users u ON u.id = k.user_id
WHERE k.key_sha256 = ? AND k.revoked_at IS NULL;

-- name: TouchAPIKeyLastUsed :exec
UPDATE api_keys SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?;

-- name: RevokeAPIKey :exec
-- Soft-revoke; scoped by user so a guessed id can't revoke another user's key.
UPDATE api_keys SET revoked_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?;

-- name: DeleteAPIKey :exec
DELETE FROM api_keys WHERE id = ? AND user_id = ?;
