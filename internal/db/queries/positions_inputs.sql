-- name: ListExecutionsForAccountSymbol :many
SELECT * FROM executions
WHERE account_id = ? AND symbol = ?
ORDER BY exec_time, id;

-- name: GetCommissionRate :one
SELECT * FROM commission_rates WHERE firm = ? AND symbol_root = ?;
