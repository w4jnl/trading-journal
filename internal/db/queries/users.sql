-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = ? LIMIT 1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ? LIMIT 1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY username;

-- name: CreateUser :one
-- Password hash may be NULL for proxy-mode users (no local password) and for
-- the bootstrap user immediately after migration (set by the CLI).
INSERT INTO users (username, email, password_hash, is_admin, display_name)
VALUES (?, ?, ?, ?, ?)
RETURNING *;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = ? WHERE id = ?;

-- name: SetUserAdmin :exec
UPDATE users SET is_admin = ? WHERE id = ?;

-- name: SetUserActive :exec
UPDATE users SET is_active = ? WHERE id = ?;

-- name: TouchUserLogin :exec
UPDATE users SET last_login_at = CURRENT_TIMESTAMP WHERE id = ?;

-- name: UpdateUserProfileFromProxy :exec
-- Proxy mode refreshes email / display_name / is_admin from headers on every
-- request. is_admin is recomputed by the caller from the groups header.
UPDATE users SET email = ?, display_name = ?, is_admin = ? WHERE id = ?;
