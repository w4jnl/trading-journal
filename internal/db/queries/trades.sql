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

-- name: CreateImportedTrade :one
-- Used by trade-level imports (TopstepX trades CSV) where the trade is
-- already fully formed in the source file. broker_trade_id is the dedup
-- key for re-imports.
INSERT INTO trades (
    account_id, symbol, symbol_root, direction, qty,
    entry_price, exit_price, entry_time, exit_time,
    gross_pnl, commission, net_pnl, status, broker_trade_id
) VALUES (
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?, 'closed', ?
)
RETURNING *;

-- name: GetTradeByBrokerID :one
SELECT * FROM trades WHERE account_id = ? AND broker_trade_id = ?;

-- name: ReassignTradesAccount :execrows
UPDATE trades SET account_id = ? WHERE account_id = ?;

-- name: ReassignExecutionsAccount :execrows
UPDATE executions SET account_id = ? WHERE account_id = ?;

-- name: ReassignJournalEntriesAccount :execrows
UPDATE journal_entries SET account_id = ? WHERE account_id = ?;

-- name: DeleteTradesForAccountSymbol :exec
DELETE FROM trades WHERE account_id = ? AND symbol = ?;

-- name: ListJournalLinksForAccountSymbol :many
-- Trade-scope journal links for this (account, symbol), captured before the
-- FIFO rebuild deletes the trades. trade_id lets the builder look up the
-- trade's first entry fill (FirstEntryExecutionForTrade), which is the
-- stable identity across rebuilds; entry_time is the fallback signal.
SELECT je.id AS journal_id, t.id AS trade_id, t.entry_time
FROM journal_entries je
JOIN trades t ON je.trade_id = t.id
WHERE t.account_id = ? AND t.symbol = ?;

-- name: FirstEntryExecutionForTrade :one
-- The execution that opened the trade: entry-role junction row with the
-- earliest fill. Execution ids survive a rebuild (trade ids do not), and a
-- fill opens exactly one trade - a flip fill is the EXIT of one trade and
-- the ENTRY of the next, so the role keeps the two apart.
SELECT te.execution_id
FROM trade_executions te
JOIN executions e ON e.id = te.execution_id
WHERE te.trade_id = ? AND te.role = 'entry'
ORDER BY e.exec_time ASC, e.id ASC
LIMIT 1;

-- name: FindTradeByEntryExecution :one
-- The (rebuilt) trade this execution opened. Scoped to the account/symbol
-- being rebuilt for safety; the execution already pins both.
SELECT te.trade_id
FROM trade_executions te
JOIN trades t ON t.id = te.trade_id
WHERE te.execution_id = ? AND te.role = 'entry' AND t.account_id = ? AND t.symbol = ?
LIMIT 1;

-- name: ClearJournalLinksForAccountSymbol :exec
UPDATE journal_entries SET trade_id = NULL
WHERE trade_id IN (SELECT t.id FROM trades t WHERE t.account_id = ? AND t.symbol = ?);

-- name: FindTradeBySignal :one
SELECT id FROM trades
WHERE account_id = ? AND symbol = ? AND entry_time = ?
LIMIT 1;

-- name: RelinkJournalEntry :exec
UPDATE journal_entries SET trade_id = ? WHERE id = ?;

-- name: ListGroupLinksForAccountSymbol :many
-- Group-journal links (journal_entry_trades M:N) pointing at trades
-- for this (account, symbol). Captured before DeleteTradesForAccountSymbol
-- runs so the FK ON DELETE CASCADE doesn't permanently lose them - we
-- re-INSERT after the FIFO rebuild lands fresh trade rows with the same
-- entry_time, mirroring the trade-scope relink dance above.
SELECT jet.journal_entry_id, jet.trade_id, t.entry_time
FROM journal_entry_trades jet
JOIN trades t ON t.id = jet.trade_id
WHERE t.account_id = ? AND t.symbol = ?;

-- name: InsertGroupLink :exec
-- Idempotent re-insert used by the position builder's relink path.
INSERT OR IGNORE INTO journal_entry_trades (journal_entry_id, trade_id)
VALUES (?, ?);

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
-- Scoped by user_id via the accounts join - the user can only fetch trades
-- on accounts they own.
SELECT t.*, a.name AS account_name, a.firm AS account_firm
FROM trades t
JOIN accounts a ON a.id = t.account_id
WHERE t.id = ? AND a.user_id = ?;

-- name: ListDistinctSymbolRoots :many
SELECT DISTINCT t.symbol_root FROM trades t
JOIN accounts a ON a.id = t.account_id
WHERE a.user_id = ?
ORDER BY t.symbol_root;

-- name: MostRecentNetPnLExitTime :one
-- Latest exit_time across closed-with-P&L trades, excluding retired accounts.
-- Used by /trades' default perf-chart fallback range. Same ORDER BY ... LIMIT
-- workaround as MostRecentClosedExitTime in dashboard.sql.
SELECT exit_time FROM trades
WHERE exit_time IS NOT NULL AND net_pnl IS NOT NULL
  AND account_id IN (SELECT id FROM accounts WHERE user_id = ? AND active = 1)
ORDER BY exit_time DESC LIMIT 1;

-- name: MostRecentNetPnLExitTimeIncludingRetired :one
-- Same as MostRecentNetPnLExitTime but for /trades?show_retired=1 -- retired
-- accounts are part of the visible set, so the fallback range must consider
-- them too.
SELECT exit_time FROM trades
WHERE exit_time IS NOT NULL AND net_pnl IS NOT NULL
  AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)
ORDER BY exit_time DESC LIMIT 1;

-- name: ListClosedTradeDeltasOrdered :many
-- Every closed trade's (id, account_id, exit_time, net_pnl) for the balance
-- recomputation paths (loadBalances + loadTradeBalances). Ordered globally
-- by exit_time so callers can group per-account in one pass. Scoped by user.
SELECT t.id, t.account_id, t.exit_time, COALESCE(t.net_pnl, 0) AS net_pnl
FROM trades t
JOIN accounts a ON a.id = t.account_id
WHERE t.status='closed' AND t.exit_time IS NOT NULL AND a.user_id = ?
ORDER BY t.exit_time ASC;

-- ListTradesFiltered + CountTradesFiltered are intentionally NOT defined here.
-- Their dynamic WHERE clauses (account, symbol, direction, date range) are
-- built in Go and run via *sql.DB -- see internal/handlers/trades.go. sqlc's
-- sqlc.narg pattern produces broken positional indexing for SQLite (each narg
-- becomes ?N which doesn't line up with the 1-based variadic args), so we
-- skip sqlc for these two queries.

-- name: ListTradesForExecution :many
-- Every trade one fill participates in. A flip fill sits in two (exit of one
-- trade, entry of the next); a fill whose qty_used sums below its qty also
-- feeds a still-open lot. Used by the trades reconcile view for its warnings.
SELECT te.trade_id, te.role, te.qty_used
FROM trade_executions te
WHERE te.execution_id = ?
ORDER BY te.trade_id;
