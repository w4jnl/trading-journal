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

-- name: GetAccountImportClaim :one
-- Returns the import format that has "claimed" the account:
--   'trades' - trades-CSV has run (commission_source flipped to 'inline')
--   'orders' - orders-CSV has produced FIFO trades (broker_trade_id NULL)
--   'none'   - no import has touched the account yet (or it was wiped)
-- Used to reject mixed TopStep imports before they double-count.
SELECT
    CASE
        WHEN a.commission_source = 'inline' THEN 'trades'
        WHEN EXISTS(SELECT 1 FROM trades t WHERE t.account_id = a.id AND t.broker_trade_id IS NULL) THEN 'orders'
        ELSE 'none'
    END AS claim
FROM accounts a
WHERE a.id = ?;
