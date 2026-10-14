-- name: InsertExecution :execrows
-- commission is the broker-reported total for the fill (TopstepX API sync);
-- NULL for CSV sources, whose trades fall back to the rate-table lookup.
-- DO NOTHING (not upsert) on purpose: execrows drives rebuild scheduling, and
-- an upsert would report rows on every sync and force constant rebuilds.
INSERT INTO executions (
    account_id, broker_exec_id, order_id, import_method, symbol, symbol_root, side, qty, price, commission, exec_time, raw_data
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT(account_id, broker_exec_id) DO NOTHING;

-- name: GetExecutionByOrderID :one
-- Several rows can share (account, order id) when a broker reports multiple
-- fills for one order (seen on IBKR CSV rows). Authoritative rows sort before
-- provisional mw_email ones so the email ingest can tell "already covered by
-- a real import" (skip) from "update my provisional snapshot".
SELECT * FROM executions
WHERE account_id = ? AND order_id = ?
ORDER BY CASE WHEN import_method = 'mw_email' THEN 1 ELSE 0 END, id
LIMIT 1;

-- name: UpdateProvisionalExecution :exec
-- MW fill emails report the order's CUMULATIVE filled qty and running avg
-- price, so a later email for the same order replaces the earlier snapshot in
-- place. Guarded to provisional rows only - never touches imported data.
UPDATE executions
SET side = ?, qty = ?, price = ?, exec_time = ?, raw_data = ?
WHERE id = ? AND import_method = 'mw_email';

-- name: ListUnorderedExecsForAccountSymbol :many
-- Non-email rows that carry NO broker order id: hand-entered fills and rows
-- from old-format CSV exports. The email ingest checks these before inserting
-- a provisional row - an order already recorded by such a "manual process"
-- must not be double-counted just because the order-id join cannot see it.
-- Rows WITH an order id are intentionally excluded: they dedup exactly, and
-- including them here could false-match a genuinely distinct same-second fill.
SELECT id, side, qty, price, exec_time FROM executions
WHERE account_id = ? AND symbol = ? AND order_id IS NULL
  AND import_method != 'mw_email' AND deleted_at IS NULL;

-- name: DeleteProvisionalByOrderID :many
-- CSV supersede: the authoritative import for this (account, order) has
-- arrived, so the provisional email snapshot goes away. Tombstoned rows are
-- kept (user explicitly deleted them - the audit trail wins). Returns the
-- symbols so the caller can schedule FIFO rebuilds even when the replacing
-- CSV row itself dedups to a no-op.
DELETE FROM executions
WHERE account_id = ? AND order_id = ? AND import_method = 'mw_email' AND deleted_at IS NULL
RETURNING symbol;

-- name: ListExecutionsByAccount :many
SELECT * FROM executions WHERE account_id = ? ORDER BY exec_time, id;

-- name: GetLastExecTimeForAccount :one
-- Newest fill on the account, used as the incremental API-sync window start.
-- Soft-deleted fills are INCLUDED on purpose: a tombstoned fill was still
-- fetched once, so the window must not re-open behind it (re-insertion is
-- blocked by the retained row anyway). ErrNoRows means "no fills yet" and
-- routes the account to the full history walk-back.
SELECT exec_time FROM executions
WHERE account_id = ?
ORDER BY exec_time DESC
LIMIT 1;

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
    account_id, broker_exec_id, symbol, symbol_root, side, qty, price, exec_time, manual, import_method
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, 1, 'manual'
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
-- Per-fill restore. Also detaches the fill from any trade-level delete group
-- it was part of, so a group only ever lists fills that are still tombstoned.
UPDATE executions
SET deleted_at = NULL, deleted_group = NULL
WHERE executions.id = ? AND executions.account_id IN (SELECT accounts.id FROM accounts WHERE accounts.user_id = ?);

-- name: SoftDeleteExecutionInGroup :execrows
-- Trade-level delete: tombstone one fill and stamp the group id shared by the
-- other fills of the same trade (restored together by UndeleteExecutionGroup).
-- Only live rows are touched: a fill already tombstoned in the fills view keeps
-- its own state, and the row count tells the caller how many fills it removed.
UPDATE executions
SET deleted_at = CURRENT_TIMESTAMP, deleted_group = sqlc.arg(deleted_group)
WHERE executions.id = sqlc.arg(id) AND executions.deleted_at IS NULL
  AND executions.account_id IN (SELECT accounts.id FROM accounts WHERE accounts.user_id = sqlc.arg(user_id));

-- name: UndeleteExecutionGroup :execrows
-- Restore every fill a trade-level delete tombstoned together. User-scoped.
UPDATE executions
SET deleted_at = NULL, deleted_group = NULL
WHERE executions.deleted_group = sqlc.arg(deleted_group)
  AND executions.account_id IN (SELECT accounts.id FROM accounts WHERE accounts.user_id = sqlc.arg(user_id));

-- name: ListDeletedGroupExecutions :many
-- Every tombstoned fill that belongs to a trade-level delete group, for the
-- "Deleted trades" section. Grouping and sums are done in Go (groups are few,
-- and sqlc types MIN/MAX over DATETIME as interface{}).
SELECT e.id, e.account_id, a.name AS account_name, a.commission_source,
       e.symbol, e.symbol_root, e.side, e.qty, e.price, e.exec_time,
       e.broker_exec_id, e.order_id, e.import_method, e.manual, e.edited_at,
       e.deleted_at, e.deleted_group
FROM executions e
JOIN accounts a ON a.id = e.account_id
WHERE a.user_id = ? AND e.deleted_group IS NOT NULL AND e.deleted_at IS NOT NULL
ORDER BY e.deleted_at DESC, e.deleted_group, e.exec_time, e.id;

-- name: ListExecutionsInDeletedGroup :many
-- Fills of one group (restore path: resolves the (account, symbol) keys to
-- rebuild before the tombstones are cleared). User-scoped.
SELECT e.id, e.account_id, e.symbol
FROM executions e
JOIN accounts a ON a.id = e.account_id
WHERE e.deleted_group = ? AND a.user_id = ?;

-- name: GetExecutionByID :one
-- One fill by id, any state (soft-deleted rows included): callers that hold
-- an id from before a supersede check whether it is still live.
SELECT * FROM executions WHERE id = ?;
