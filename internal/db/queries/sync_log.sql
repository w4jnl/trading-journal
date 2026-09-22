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
-- Newest row per sync_type for the user (the dashboard's last-sync panel).
-- Grouped MAX(id) instead of a correlated subquery: the old form rescanned
-- the whole per-user history once per row (quadratic in table size).
SELECT s.* FROM sync_log s
JOIN (
    SELECT MAX(l.id) AS id FROM sync_log l WHERE l.user_id = ? GROUP BY l.sync_type
) latest ON latest.id = s.id
ORDER BY s.synced_at DESC;

-- name: ListLastAPISyncPerAccount :many
-- Latest API-sync row per account for one sync_type ('topstepx_api' /
-- 'ibkr_api'), for settings/accounts display. Grouped MAX(id) for the same
-- reason as ListLastSyncByType.
SELECT s.account_id, s.synced_at, s.status, s.message, s.execs_added
FROM sync_log s
JOIN (
    SELECT MAX(l.id) AS id FROM sync_log l
    WHERE l.user_id = ? AND l.sync_type = ? AND l.account_id IS NOT NULL
    GROUP BY l.account_id
) latest ON latest.id = s.id
ORDER BY s.synced_at DESC;

-- name: PurgeSyncLogBatch :execrows
-- Retention for API polling rows (topstepx_api / ibkr_api). Each poll writes
-- one row per account, thousands a day, so without this the table grows
-- without bound and every reader slows down. Deleted: rows older than the
-- no-op cutoff that added nothing and did not error, and rows older than the
-- event cutoff regardless. Always kept: CSV import rows (an audit trail of
-- files), the newest row per user and type (dashboard panel) and the newest
-- per user, type and account (settings page). Batched so the write lock
-- stays short; the caller loops until a batch deletes nothing.
DELETE FROM sync_log WHERE id IN (
    SELECT id FROM sync_log
    WHERE sync_type IN ('topstepx_api', 'ibkr_api')
      AND (
            (synced_at < datetime('now', sqlc.arg(noop_age))
             AND execs_added = 0 AND trades_added = 0 AND status <> 'error')
         OR synced_at < datetime('now', sqlc.arg(event_age))
      )
      AND id NOT IN (SELECT MAX(id) FROM sync_log GROUP BY user_id, sync_type)
      AND id NOT IN (SELECT MAX(id) FROM sync_log WHERE account_id IS NOT NULL GROUP BY user_id, sync_type, account_id)
    LIMIT sqlc.arg(batch)
);
