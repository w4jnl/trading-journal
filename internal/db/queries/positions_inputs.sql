-- name: ListExecutionsForAccountSymbol :many
SELECT * FROM executions
WHERE account_id = ? AND symbol = ?
ORDER BY exec_time, id;

-- name: GetCommissionRate :one
SELECT * FROM commission_rates
WHERE asset_class = ? AND firm = ? AND size_class = ?;

-- name: ListDistinctSymbolsForAccount :many
SELECT DISTINCT symbol FROM executions WHERE account_id = ? ORDER BY symbol;
