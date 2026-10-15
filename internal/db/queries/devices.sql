-- Phones registered for push (internal/push).

-- name: UpsertDevice :one
-- A token registers once; a second registration (a new app build, a new
-- name) updates the row and marks it seen.
INSERT INTO devices (user_id, token, name, sandbox, bundle_id, prefs)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(token) DO UPDATE SET
    user_id = excluded.user_id, name = excluded.name, sandbox = excluded.sandbox,
    bundle_id = excluded.bundle_id, prefs = excluded.prefs, last_seen_at = CURRENT_TIMESTAMP
RETURNING *;

-- name: GetDeviceByToken :one
SELECT * FROM devices WHERE token = ? AND user_id = ?;

-- name: ListDevicesForUser :many
SELECT * FROM devices WHERE user_id = ? ORDER BY created_at, id;

-- name: ListDevicesWithStartToken :many
-- The user's phones that can start the Session activity by push.
SELECT * FROM devices WHERE user_id = ? AND activity_start_token IS NOT NULL AND activity_start_token <> '';

-- name: UpdateDevicePrefs :execrows
UPDATE devices SET prefs = ?, last_seen_at = CURRENT_TIMESTAMP WHERE token = ? AND user_id = ?;

-- name: SetDeviceActivityStart :execrows
UPDATE devices SET activity_start_token = ?, last_seen_at = CURRENT_TIMESTAMP WHERE token = ? AND user_id = ?;

-- name: TouchDevice :exec
UPDATE devices SET last_seen_at = CURRENT_TIMESTAMP WHERE id = ?;

-- name: DeleteDevice :execrows
DELETE FROM devices WHERE token = ? AND user_id = ?;

-- name: DeleteDeviceByID :exec
-- The service forgets a device Apple no longer knows (410), whoever's it is.
DELETE FROM devices WHERE id = ?;

-- name: DeleteDeviceActivities :exec
DELETE FROM device_activities WHERE device_id = ?;

-- name: InsertDeviceActivity :exec
INSERT OR REPLACE INTO device_activities (device_id, activity_id, token, kind) VALUES (?, ?, ?, ?);

-- name: ListDeviceActivities :many
SELECT * FROM device_activities WHERE device_id = ? ORDER BY created_at, activity_id;

-- name: DeleteDeviceActivityByToken :exec
DELETE FROM device_activities WHERE token = ?;

-- name: InsertDeviceSession :execrows
-- 0 rows: the day's session was started on this device already.
INSERT OR IGNORE INTO device_sessions (device_id, day) VALUES (?, ?);

-- name: DeleteOldDeviceSessions :exec
DELETE FROM device_sessions WHERE day < ?;

-- name: ListUsersWithDevices :many
SELECT DISTINCT user_id FROM devices ORDER BY user_id;
