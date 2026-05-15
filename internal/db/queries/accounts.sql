-- name: GetAccountByRef :one
SELECT * FROM accounts WHERE import_source = ? AND account_ref = ?;

-- name: CreateAccount :one
INSERT INTO accounts (
    name, firm, import_source, account_ref, type, currency, needs_setup
) VALUES (
    ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

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
