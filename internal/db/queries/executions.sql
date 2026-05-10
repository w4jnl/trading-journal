-- name: InsertExecution :execrows
INSERT INTO executions (
    account_id, broker_exec_id, symbol, symbol_root, side, qty, price, exec_time, raw_data
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?
)
ON CONFLICT(account_id, broker_exec_id) DO NOTHING;

-- name: ListExecutionsByAccount :many
SELECT * FROM executions WHERE account_id = ? ORDER BY exec_time, id;
