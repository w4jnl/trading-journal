-- name: CreateSyncLog :one
INSERT INTO sync_log (
    user_id, account_id, sync_type, status, source_file, trades_added, execs_added, message
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: ListSyncLogRecent :many
SELECT * FROM sync_log ORDER BY synced_at DESC LIMIT ?;

-- name: ListLastSyncByType :many
SELECT * FROM sync_log s1
WHERE id = (SELECT MAX(id) FROM sync_log WHERE sync_type = s1.sync_type)
ORDER BY synced_at DESC;
