-- name: GetStrategyPlaybook :one
SELECT * FROM strategy_playbooks WHERE user_id = ?;

-- name: UpsertStrategyPlaybook :exec
INSERT INTO strategy_playbooks (user_id, body, updated_at)
VALUES (?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(user_id) DO UPDATE SET
    body       = excluded.body,
    updated_at = CURRENT_TIMESTAMP;
