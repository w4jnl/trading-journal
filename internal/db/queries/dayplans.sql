-- Pre-market plans (internal/dayplan). ASCII-only comments: sqlc
-- miscounts multi-byte bytes and corrupts the next query.

-- name: GetDayPlan :one
SELECT * FROM day_plans WHERE user_id = ? AND day = ?;

-- name: UpsertDayPlan :exec
INSERT INTO day_plans (user_id, day, bias, levels, max_trades, stop_time, max_loss, notes, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, day) DO UPDATE SET
    bias = excluded.bias, levels = excluded.levels, max_trades = excluded.max_trades,
    stop_time = excluded.stop_time, max_loss = excluded.max_loss, notes = excluded.notes,
    updated_at = excluded.updated_at;

-- name: DeleteDayPlan :exec
DELETE FROM day_plans WHERE user_id = ? AND day = ?;

-- name: ListDayPlansBetween :many
-- Plans in an inclusive day range (the weekly digest).
SELECT * FROM day_plans WHERE user_id = ? AND day >= ? AND day <= ? ORDER BY day;
