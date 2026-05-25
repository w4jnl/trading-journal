-- Queries powering the / dashboard. All four bake in the "retired accounts
-- are hidden" rule by joining `accounts.active = 1` via an IN-subquery, so
-- the rule lives next to the SQL rather than being glued in at the call site.
-- Each accounts subquery is also scoped by user_id so users see only their
-- own data.

-- name: DashboardDailyNetPnL :many
-- Per-closed-trade exit_time + net_pnl within [from, to). Drives the 60-day
-- equity curve and the trailing-strip aggregates.
SELECT exit_time, net_pnl
FROM trades
WHERE status='closed' AND exit_time IS NOT NULL AND net_pnl IS NOT NULL
  AND exit_time >= ? AND exit_time < ?
  AND account_id IN (SELECT id FROM accounts WHERE user_id = ? AND active = 1)
ORDER BY exit_time ASC;

-- name: CountClosedTradesInRange :one
-- Used by pickDashboardDate to decide whether today already has activity.
SELECT COUNT(*) FROM trades
WHERE status='closed' AND exit_time >= ? AND exit_time < ?
  AND account_id IN (SELECT id FROM accounts WHERE user_id = ? AND active = 1);

-- name: MostRecentClosedExitTime :one
-- Latest closed-trade exit_time across non-retired accounts. Fallback used
-- by pickDashboardDate when today has no activity. ORDER BY ... LIMIT 1 is
-- intentional -- MAX() loses the column's TIMESTAMP type under
-- modernc.org/sqlite and refuses to scan back into time.Time.
SELECT exit_time FROM trades
WHERE status='closed' AND exit_time IS NOT NULL
  AND account_id IN (SELECT id FROM accounts WHERE user_id = ? AND active = 1)
ORDER BY exit_time DESC LIMIT 1;

-- name: ListClosedTradesInRange :many
-- Closed-trade rows whose exit falls in [from, to). Columns chosen to match
-- dashboardStatsFor's aggregator: direction + qty for the long/short split,
-- gross/commission/net for the P&L tiles.
SELECT direction, qty, gross_pnl, commission, net_pnl
FROM trades
WHERE status='closed' AND exit_time IS NOT NULL
  AND exit_time >= ? AND exit_time < ?
  AND account_id IN (SELECT id FROM accounts WHERE user_id = ? AND active = 1);
