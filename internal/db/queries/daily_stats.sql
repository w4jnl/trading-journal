-- name: DeleteDailyStatsForAccount :exec
DELETE FROM daily_stats WHERE account_id = ?;

-- name: CreateDailyStat :exec
INSERT INTO daily_stats (
    account_id, stat_date, trades_count, wins, losses,
    gross_pnl, net_pnl, commission, largest_win, largest_loss
) VALUES (
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?
);

-- name: ListDailyStatsForAccount :many
SELECT * FROM daily_stats WHERE account_id = ? ORDER BY stat_date;

-- name: ListDailyStatsForRange :many
-- Scoped to a single user by joining accounts.user_id. Used by the journal
-- day/week aggregators and any other view that walks "all my activity in
-- this date window".
SELECT ds.* FROM daily_stats ds
JOIN accounts a ON a.id = ds.account_id
WHERE a.user_id = ? AND ds.stat_date >= ? AND ds.stat_date < ?
ORDER BY ds.stat_date, ds.account_id;
