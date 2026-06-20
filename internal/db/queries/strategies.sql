-- name: ListStrategies :many
-- All of the user's strategies, active first then archived (retired), each by name.
SELECT * FROM strategies WHERE user_id = ? ORDER BY (archived_at IS NOT NULL), name;

-- name: ListActiveStrategies :many
-- Only non-archived strategies: the set offered for assignment and shown to AI analyzers.
SELECT * FROM strategies WHERE user_id = ? AND archived_at IS NULL ORDER BY name;

-- name: GetStrategy :one
SELECT * FROM strategies WHERE id = ? AND user_id = ?;

-- name: CreateStrategy :one
INSERT INTO strategies (user_id, name, body)
VALUES (?, ?, ?)
RETURNING *;

-- name: UpdateStrategy :exec
UPDATE strategies
SET name = ?, body = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;

-- name: ArchiveStrategy :exec
UPDATE strategies
SET archived_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;

-- name: UnarchiveStrategy :exec
UPDATE strategies
SET archived_at = NULL, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;

-- name: DeleteStrategy :exec
DELETE FROM strategies WHERE id = ? AND user_id = ?;
