-- name: ListStrategies :many
SELECT * FROM strategies WHERE user_id = ? ORDER BY name;

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

-- name: DeleteStrategy :exec
DELETE FROM strategies WHERE id = ? AND user_id = ?;
