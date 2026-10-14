-- Copy sets (internal/copyset): derived from closed trades, rebuilt whole
-- per user. Comments here must stay ASCII-only (sqlc byte counting).

-- name: ListClosedTradesForSets :many
SELECT t.id, t.account_id, t.symbol, t.symbol_root, t.direction, t.qty,
       t.entry_price, t.exit_price, t.entry_time, t.exit_time,
       t.gross_pnl, t.commission, t.net_pnl, a.copy_group
FROM trades t
JOIN accounts a ON a.id = t.account_id
WHERE a.user_id = ? AND t.status = 'closed' AND t.exit_time IS NOT NULL
ORDER BY t.entry_time, t.id;

-- name: ListTradeSetMembersWithPlans :many
-- Every member of the user's sets with its plan and, when the plan came
-- from an open position, whether it was set for the whole decision
-- (position_plans.set_plan). The inherit step (positions.InheritSetPlans)
-- gives unplanned legs the set's plan.
SELECT tsm.set_id, t.id, t.account_id, t.symbol, t.symbol_root, t.direction, t.qty, t.entry_price,
       t.planned_stop, t.planned_risk, t.planned_target,
       pp.id AS plan_id, pp.set_plan, pp.thesis, pp.source
FROM trade_set_members tsm
JOIN trade_sets ts ON ts.id = tsm.set_id
JOIN trades t ON t.id = tsm.trade_id
LEFT JOIN position_plans pp ON pp.trade_id = t.id
WHERE ts.user_id = ?
ORDER BY tsm.set_id, t.entry_time, t.id;

-- name: DeleteTradeSetsForUser :exec
DELETE FROM trade_sets WHERE user_id = ?;

-- name: InsertTradeSet :one
INSERT INTO trade_sets (user_id, cluster_key, symbol_root, direction, entry_time, exit_time,
                        members, accounts, qty, entry_avg, entry_spread, exit_avg,
                        gross_pnl, commission, net_pnl)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: InsertTradeSetMember :exec
INSERT INTO trade_set_members (trade_id, set_id) VALUES (?, ?);

-- name: GetTradeSet :one
SELECT * FROM trade_sets WHERE id = ? AND user_id = ?;

-- name: CountTradeSetsForUser :one
SELECT COUNT(*) FROM trade_sets WHERE user_id = ?;

-- name: GetAccountOwner :one
SELECT user_id FROM accounts WHERE id = ?;

-- name: GetTradeSetMembership :one
-- The set a trade belongs to and how many members it has (API rows).
SELECT m.set_id, (SELECT COUNT(*) FROM trade_set_members m2 WHERE m2.set_id = m.set_id) AS set_size
FROM trade_set_members m WHERE m.trade_id = ?;
