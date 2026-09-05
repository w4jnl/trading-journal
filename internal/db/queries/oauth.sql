-- OAuth 2.1 authorization server tables (see migration 20260905120000).
-- Comments here are ASCII-only on purpose: sqlc miscounts multi-byte
-- characters in comments and silently truncates the following query.

-- ---------------------------------------------------------------- clients

-- name: CreateOAuthClient :one
INSERT INTO oauth_clients (client_id, client_name, redirect_uris)
VALUES (?, ?, ?)
RETURNING *;

-- name: GetOAuthClientByClientID :one
SELECT * FROM oauth_clients WHERE client_id = ?;

-- ----------------------------------------------------------------- grants

-- name: CreateOAuthGrant :one
INSERT INTO oauth_grants (user_id, client_id, scope, resource)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetOAuthGrantByID :one
SELECT * FROM oauth_grants WHERE id = ?;

-- name: ListOAuthGrantsForUser :many
-- Newest first; includes revoked grants so the settings UI can show their
-- state, mirroring ListAPIKeysForUser.
SELECT g.id,
       g.client_id,
       COALESCE(c.client_name, '') AS client_name,
       c.redirect_uris,
       g.scope,
       g.resource,
       g.created_at,
       g.last_used_at,
       g.revoked_at
FROM oauth_grants g
JOIN oauth_clients c ON c.client_id = g.client_id
WHERE g.user_id = ?
ORDER BY g.created_at DESC, g.id DESC;

-- name: RevokeOAuthGrantForUser :execrows
-- User-initiated revoke; scoped by user so a guessed id cannot revoke another
-- user's grant. Tokens under the grant fail on their next use via the joins.
UPDATE oauth_grants SET revoked_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ? AND revoked_at IS NULL;

-- name: RevokeOAuthGrantByID :exec
-- Server-side family revocation (replayed code or reused refresh token).
UPDATE oauth_grants SET revoked_at = CURRENT_TIMESTAMP
WHERE id = ? AND revoked_at IS NULL;

-- name: TouchOAuthGrantLastUsed :exec
UPDATE oauth_grants SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?;

-- name: DeleteOAuthGrantForUser :exec
-- Only revoked grants can be deleted from the UI; live ones must be revoked
-- first so the two-step matches the API-keys card.
DELETE FROM oauth_grants WHERE id = ? AND user_id = ? AND revoked_at IS NOT NULL;

-- ------------------------------------------------------- authorization codes

-- name: CreateOAuthAuthCode :one
INSERT INTO oauth_auth_codes (code_sha256, grant_id, client_id, user_id, redirect_uri,
                              code_challenge, code_challenge_method, resource, scope, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetOAuthAuthCodeBySHA256 :one
SELECT * FROM oauth_auth_codes WHERE code_sha256 = ?;

-- name: ConsumeOAuthAuthCode :execrows
-- Single-use gate: exactly one caller sees 1 row affected; a replay sees 0.
UPDATE oauth_auth_codes SET consumed_at = CURRENT_TIMESTAMP
WHERE id = ? AND consumed_at IS NULL;

-- ------------------------------------------------------------ access tokens

-- name: CreateOAuthAccessToken :one
INSERT INTO oauth_access_tokens (token_sha256, grant_id, client_id, user_id, resource, scope, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetOAuthAccessTokenBySHA256 :one
-- /mcp auth lookup. Same shape as GetAPIKeyBySHA256 so the middleware builds
-- the context user in one round-trip. Only tokens that are unrevoked, whose
-- grant is unrevoked, and that have not expired resolve; is_active lets the
-- middleware also reject deactivated users.
SELECT t.id        AS token_id,
       t.grant_id  AS grant_id,
       t.client_id AS client_id,
       t.user_id   AS user_id,
       t.resource  AS resource,
       t.scope     AS scope,
       u.username,
       COALESCE(u.display_name, u.username) AS display_name,
       COALESCE(u.email, '')                AS email,
       u.is_admin,
       u.is_active
FROM oauth_access_tokens t
JOIN oauth_grants g ON g.id = t.grant_id
JOIN users u        ON u.id = t.user_id
WHERE t.token_sha256 = ?
  AND t.revoked_at IS NULL
  AND g.revoked_at IS NULL
  AND t.expires_at > CURRENT_TIMESTAMP;

-- name: TouchOAuthAccessTokenLastUsed :exec
UPDATE oauth_access_tokens SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?;

-- ----------------------------------------------------------- refresh tokens

-- name: CreateOAuthRefreshToken :one
INSERT INTO oauth_refresh_tokens (token_sha256, grant_id, client_id, user_id, resource, scope, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetOAuthRefreshTokenBySHA256 :one
-- Returns the row even when revoked/rotated so the token endpoint can tell
-- "reuse of a rotated token" (revoke the family) from "unknown token".
SELECT r.id,
       r.token_sha256,
       r.grant_id,
       r.client_id,
       r.user_id,
       r.resource,
       r.scope,
       r.expires_at,
       r.revoked_at,
       r.rotated_to,
       g.revoked_at AS grant_revoked_at
FROM oauth_refresh_tokens r
JOIN oauth_grants g ON g.id = r.grant_id
WHERE r.token_sha256 = ?;

-- name: RotateOAuthRefreshToken :execrows
-- Rotation gate: 1 row affected means this caller retired the token; 0 means
-- it was already rotated (reuse -> revoke the grant).
UPDATE oauth_refresh_tokens SET revoked_at = CURRENT_TIMESTAMP, rotated_to = ?
WHERE id = ? AND revoked_at IS NULL;

-- ------------------------------------------------------------------ janitor

-- name: DeleteExpiredOAuthAuthCodes :exec
DELETE FROM oauth_auth_codes WHERE expires_at <= CURRENT_TIMESTAMP;

-- name: DeleteExpiredOAuthAccessTokens :exec
DELETE FROM oauth_access_tokens WHERE expires_at <= CURRENT_TIMESTAMP;

-- name: DeleteExpiredOAuthRefreshTokens :exec
DELETE FROM oauth_refresh_tokens WHERE expires_at <= CURRENT_TIMESTAMP;
