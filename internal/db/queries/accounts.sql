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
