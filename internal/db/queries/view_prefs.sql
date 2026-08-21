-- Per-user sticky view state (see the user_view_prefs migration). Comments
-- here must stay ASCII-only: sqlc miscounts multi-byte bytes in comments and
-- silently corrupts the NEXT query in the file.

-- name: GetViewPref :one
SELECT value FROM user_view_prefs WHERE user_id = ? AND page = ?;

-- name: UpsertViewPref :exec
INSERT INTO user_view_prefs (user_id, page, value, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (user_id, page) DO UPDATE SET
    value = excluded.value,
    updated_at = excluded.updated_at;
