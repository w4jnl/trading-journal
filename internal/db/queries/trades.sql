-- name: CreateTrade :one
INSERT INTO trades (
    account_id, symbol, symbol_root, direction, qty,
    entry_price, exit_price, entry_time, exit_time,
    gross_pnl, commission, net_pnl, status
) VALUES (
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?, ?
)
RETURNING *;

-- name: DeleteTradesForAccountSymbol :exec
DELETE FROM trades WHERE account_id = ? AND symbol = ?;

-- name: ListTradesForAccount :many
SELECT * FROM trades WHERE account_id = ? ORDER BY entry_time, id;

-- name: ListTradesByDate :many
SELECT * FROM trades
WHERE account_id = ? AND status = 'closed' AND exit_time IS NOT NULL
ORDER BY exit_time, id;

-- name: LinkTradeExecution :exec
INSERT INTO trade_executions (trade_id, execution_id, role, qty_used)
VALUES (?, ?, ?, ?);

-- name: ListExecutionsForTrade :many
SELECT e.*, te.role, te.qty_used
FROM executions e
JOIN trade_executions te ON te.execution_id = e.id
WHERE te.trade_id = ?
ORDER BY te.role DESC, e.exec_time, e.id;
