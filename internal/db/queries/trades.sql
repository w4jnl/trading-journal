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
ORDER BY te.role ASC, e.exec_time, e.id;

-- name: GetTradeByID :one
SELECT t.*, a.name AS account_name, a.firm AS account_firm
FROM trades t
JOIN accounts a ON a.id = t.account_id
WHERE t.id = ?;

-- name: ListDistinctSymbolRoots :many
SELECT DISTINCT symbol_root FROM trades ORDER BY symbol_root;

-- ListTradesFiltered + CountTradesFiltered are intentionally NOT defined here.
-- Their dynamic WHERE clauses (account, symbol, direction, date range) are
-- built in Go and run via *sql.DB — see internal/handlers/trades.go. sqlc's
-- sqlc.narg pattern produces broken positional indexing for SQLite (each narg
-- becomes ?N which doesn't line up with the 1-based variadic args), so we
-- skip sqlc for these two queries.
