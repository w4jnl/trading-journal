-- name: ListExecutionsForAccountSymbol :many
-- FIFO builder input. Soft-deleted rows (deleted_at set via the reconcile editor)
-- are excluded so a manual delete actually removes the fill from trade reconstruction.
SELECT * FROM executions
WHERE account_id = ? AND symbol = ? AND deleted_at IS NULL
ORDER BY exec_time, id;

-- name: ListCommissionRateVersionsForCell :many
-- All effective-dated rate versions for one commission cell, oldest first. The
-- builder loads these once per (account, symbol) replay and picks, per fill, the
-- version whose effective_from is the greatest <= that fill's exec_time.
SELECT effective_from, per_contract_per_side FROM commission_rates
WHERE asset_class = ? AND firm = ? AND size_class = ?
ORDER BY effective_from ASC;

-- name: ListDistinctSymbolsForAccount :many
SELECT DISTINCT symbol FROM executions WHERE account_id = ? ORDER BY symbol;
