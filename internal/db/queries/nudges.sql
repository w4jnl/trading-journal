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
-- A page took the nudges: delivered, and seen unless already seen elsewhere.
UPDATE nudges SET delivered_at = CURRENT_TIMESTAMP, seen_at = COALESCE(seen_at, CURRENT_TIMESTAMP)
WHERE user_id = ? AND delivered_at IS NULL AND id <= ?;

-- name: MarkNudgeSeen :execrows
-- The trader acknowledged one nudge (the phone); 0 rows = not theirs or seen before.
UPDATE nudges SET seen_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND seen_at IS NULL;

-- name: MarkNudgesSeen :execrows
UPDATE nudges SET seen_at = CURRENT_TIMESTAMP WHERE user_id = ? AND seen_at IS NULL AND id IN (sqlc.slice(ids));

-- name: CountUnseenNudges :one
-- Today's nudges the trader has not acknowledged, created since a moment
-- (older ones have passed): the phone's badge.
SELECT COUNT(*) FROM nudges WHERE user_id = ? AND day = ? AND seen_at IS NULL AND created_at >= ?;

-- name: ListNudgesForDayDesc :many
SELECT * FROM nudges WHERE user_id = ? AND day = ? ORDER BY id DESC;

-- name: ListNudgesForDay :many
SELECT * FROM nudges WHERE user_id = ? AND day = ? ORDER BY id;
