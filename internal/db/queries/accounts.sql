-- name: GetAccountByRef :one
SELECT * FROM accounts WHERE import_source = ? AND account_ref = ?;

-- name: CreateAccount :one
INSERT INTO accounts (
    name, firm, import_source, account_ref, type, currency, needs_setup, commission_source
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: SetAccountCommissionSource :exec
-- Flips an account between rate-table lookup and CSV-inline commissions.
-- Called from the importer when a trades-shaped CSV (TopstepX trades) lands
-- so the FIFO rebuild path skips the account afterwards.
UPDATE accounts SET commission_source = ? WHERE id = ?;

-- name: ListAccounts :many
SELECT * FROM accounts ORDER BY name;

-- name: GetAccountByID :one
SELECT * FROM accounts WHERE id = ?;

-- name: UpdateAccount :one
UPDATE accounts
SET name = ?, firm = ?, type = ?, currency = ?, active = ?, needs_setup = ?
WHERE id = ?
RETURNING *;

-- name: ListRetiredAccountIDs :many
-- IDs of retired accounts (active = 0). Used by handlers that need a quick
-- set lookup to skip retired-account rows in already-fetched daily_stats
-- or accounts lists.
SELECT id FROM accounts WHERE active = 0;
