-- name: CreateSession :exec
INSERT INTO sessions (id, user_id, expires_at, user_agent, ip)
VALUES (?, ?, ?, ?, ?);

-- name: GetSession :one
-- Returns the session plus the owning user in one round-trip. Filtering by
-- expires_at > now means a stale cookie reads as ErrNoRows and the caller
-- can drop the cookie without a second query.
SELECT s.id           AS session_id,
       s.expires_at,
       u.id           AS user_id,
       u.username,
       COALESCE(u.display_name, u.username) AS display_name,
       COALESCE(u.email, '')                AS email,
       u.is_admin,
       u.is_active
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = ? AND s.expires_at > CURRENT_TIMESTAMP;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = ?;

-- name: DeleteSessionsForUser :exec
-- Used by password reset / deactivate to force re-login.
DELETE FROM sessions WHERE user_id = ?;

-- name: PurgeExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= CURRENT_TIMESTAMP;
