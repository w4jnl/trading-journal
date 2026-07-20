-- name: CreateIbkrGateway :one
INSERT INTO ibkr_gateways (user_id, name, url, tls_insecure, enabled)
VALUES (?, ?, ?, ?, 1)
RETURNING *;

-- name: ListIbkrGatewaysForUser :many
SELECT * FROM ibkr_gateways WHERE user_id = ? ORDER BY name;

-- name: ListEnabledIbkrGateways :many
-- All users' enabled gateways: drives the keep-alive tickle loop and the
-- scheduler fan-out. Owner must still be an active journal user.
SELECT g.* FROM ibkr_gateways g
JOIN users u ON u.id = g.user_id
WHERE g.enabled = 1 AND u.is_active = 1
ORDER BY g.user_id, g.name;

-- name: GetIbkrGateway :one
SELECT * FROM ibkr_gateways WHERE id = ? AND user_id = ?;

-- name: SetIbkrGatewayEnabled :exec
UPDATE ibkr_gateways SET enabled = ? WHERE id = ? AND user_id = ?;

-- name: DeleteIbkrGateway :exec
DELETE FROM ibkr_gateways WHERE id = ? AND user_id = ?;

-- name: SetAccountLiveSyncSince :exec
-- Sets (or resets) the CSV/API cutover instant for a live-synced account.
UPDATE accounts SET live_sync_since = ? WHERE id = ? AND user_id = ?;
