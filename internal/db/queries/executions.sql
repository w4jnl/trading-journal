-- name: InsertExecution :execrows
INSERT INTO executions (
    account_id, broker_exec_id, symbol, symbol_root, side, qty, price, exec_time, raw_data
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT(account_id, broker_exec_id) DO NOTHING;

-- name: ListExecutionsByAccount :many
SELECT * FROM executions WHERE account_id = ? ORDER BY exec_time, id;

-- name: GetExecutionByBrokerID :one
SELECT * FROM executions WHERE account_id = ? AND broker_exec_id = ?;

-- name: ListOpenPositions :many
-- Per (account, contract) net signed quantity, only where it does not net flat. A
-- nonzero net means an open (or carried, or data-gap) position. Soft-deleted fills are
-- excluded. Scoped to the user's own accounts. Inline (TopstepX) accounts net flat by
-- construction, so they never appear here.
SELECT a.id AS account_id, a.name AS account_name, a.account_ref, a.firm,
       e.symbol, e.symbol_root,
       CAST(SUM(CASE WHEN e.side = 'buy' THEN e.qty ELSE -e.qty END) AS REAL) AS net_qty,
       MAX(e.exec_time) AS last_exec_time
FROM executions e
JOIN accounts a ON a.id = e.account_id
WHERE a.user_id = ? AND e.deleted_at IS NULL
GROUP BY e.account_id, e.symbol
HAVING SUM(CASE WHEN e.side = 'buy' THEN e.qty ELSE -e.qty END) <> 0
ORDER BY a.name, e.symbol;

-- name: CountOpenPositions :one
-- Cheap count for the always-on nav badge (same rule as ListOpenPositions).
SELECT COUNT(*) FROM (
    SELECT 1
    FROM executions e
    JOIN accounts a ON a.id = e.account_id
    WHERE a.user_id = ? AND e.deleted_at IS NULL
    GROUP BY e.account_id, e.symbol
    HAVING SUM(CASE WHEN e.side = 'buy' THEN e.qty ELSE -e.qty END) <> 0
);

-- name: GetExecutionForUser :one
-- One execution plus the owning account's ref + commission_source (for the inline
-- guard), scoped so a user can only read their own.
SELECT e.*, a.account_ref, a.commission_source, a.user_id AS owner_id
FROM executions e
JOIN accounts a ON a.id = e.account_id
WHERE e.id = ? AND a.user_id = ?;

-- name: InsertManualExecution :one
-- Hand-created fill from the reconcile editor. broker_exec_id is a synthetic MANUAL-...
-- id the caller mints; manual=1 tags it. Returns the row (for the rebuild that follows).
INSERT INTO executions (
    account_id, broker_exec_id, symbol, symbol_root, side, qty, price, exec_time, manual
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, 1
)
RETURNING *;

-- name: UpdateExecution :exec
-- Edit an existing fill (manual or imported), scoped to the user via the account. Sets
-- edited_at as an audit marker. The caller rebuilds the affected (account, symbol) after.
UPDATE executions
SET symbol = ?, symbol_root = ?, side = ?, qty = ?, price = ?, exec_time = ?,
    edited_at = CURRENT_TIMESTAMP
WHERE executions.id = ? AND executions.account_id IN (SELECT accounts.id FROM accounts WHERE accounts.user_id = ?);

-- name: SoftDeleteExecution :exec
-- Tombstone a fill (retained so re-import cannot resurrect it). User-scoped.
UPDATE executions
SET deleted_at = CURRENT_TIMESTAMP
WHERE executions.id = ? AND executions.account_id IN (SELECT accounts.id FROM accounts WHERE accounts.user_id = ?);

-- name: UndeleteExecution :exec
UPDATE executions
SET deleted_at = NULL
WHERE executions.id = ? AND executions.account_id IN (SELECT accounts.id FROM accounts WHERE accounts.user_id = ?);
