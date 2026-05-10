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
SELECT * FROM daily_stats
WHERE stat_date >= ? AND stat_date < ?
ORDER BY stat_date, account_id;
