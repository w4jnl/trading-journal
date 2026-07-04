-- name: ListExecutionsForAccountSymbol :many
-- FIFO builder input. Soft-deleted rows (deleted_at set via the reconcile editor)
-- are excluded so a manual delete actually removes the fill from trade reconstruction.
SELECT * FROM executions
WHERE account_id = ? AND symbol = ? AND deleted_at IS NULL
ORDER BY exec_time, id;

-- name: GetCommissionRate :one
SELECT * FROM commission_rates
WHERE asset_class = ? AND firm = ? AND size_class = ?;

-- name: ListDistinctSymbolsForAccount :many
SELECT DISTINCT symbol FROM executions WHERE account_id = ? ORDER BY symbol;
