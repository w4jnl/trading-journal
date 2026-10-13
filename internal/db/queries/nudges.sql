-- name: InsertNudge :execrows
-- Said once per (user, day, key, ref): 0 rows = already said.
INSERT OR IGNORE INTO nudges (user_id, day, key, ref, severity, title, detail, lesson, href)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListUndeliveredNudges :many
-- Nudges no page has shown yet, created since a moment (older ones are stale).
SELECT * FROM nudges
WHERE user_id = ? AND delivered_at IS NULL AND created_at >= ?
ORDER BY id;

-- name: MarkNudgesDelivered :exec
UPDATE nudges SET delivered_at = CURRENT_TIMESTAMP
WHERE user_id = ? AND delivered_at IS NULL AND id <= ?;

-- name: ListNudgesForDay :many
SELECT * FROM nudges WHERE user_id = ? AND day = ? ORDER BY id;
