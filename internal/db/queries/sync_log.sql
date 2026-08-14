-- name: CreateSyncLog :one
INSERT INTO sync_log (
    user_id, account_id, sync_type, status, source_file, trades_added, execs_added, message
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: ListSyncLogRecent :many
SELECT * FROM sync_log WHERE user_id = ? ORDER BY synced_at DESC LIMIT ?;

-- name: ListLastSyncByType :many
SELECT * FROM sync_log s1
WHERE s1.user_id = ?
  AND s1.id = (SELECT MAX(s2.id) FROM sync_log s2 WHERE s2.sync_type = s1.sync_type AND s2.user_id = s1.user_id)
ORDER BY s1.synced_at DESC;

-- name: ListLastAPISyncPerAccount :many
-- Latest API-sync row per account for one sync_type ('topstepx_api' /
-- 'ibkr_api'), for settings/accounts display.
SELECT s1.account_id, s1.synced_at, s1.status, s1.message, s1.execs_added
FROM sync_log s1
WHERE s1.user_id = ? AND s1.sync_type = ? AND s1.account_id IS NOT NULL
  AND s1.id = (SELECT MAX(s2.id) FROM sync_log s2
               WHERE s2.user_id = s1.user_id AND s2.sync_type = s1.sync_type
                 AND s2.account_id = s1.account_id)
ORDER BY s1.synced_at DESC;

