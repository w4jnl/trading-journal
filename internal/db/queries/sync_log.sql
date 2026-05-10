-- name: CreateSyncLog :one
INSERT INTO sync_log (
    account_id, sync_type, status, source_file, trades_added, execs_added, message
) VALUES (
    ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: ListSyncLogRecent :many
SELECT * FROM sync_log ORDER BY synced_at DESC LIMIT ?;
