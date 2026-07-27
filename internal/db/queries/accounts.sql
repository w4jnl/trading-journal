-- name: GetAccountByRef :one
-- Scoped by user_id so two users with the same broker account_ref under the
-- same import_source don't collide. Unique constraint is
-- (user_id, import_source, account_ref).
SELECT * FROM accounts WHERE user_id = ? AND import_source = ? AND account_ref = ?;

-- name: CreateAccount :one
INSERT INTO accounts (
    user_id, name, firm, import_source, account_ref, type, currency, needs_setup, commission_source
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: SetAccountCommissionSource :exec
-- Flips an account between rate-table lookup and CSV-inline commissions.
-- Scoped by user_id so a handler can't be tricked into flipping someone
-- else's account via a guessed id.
UPDATE accounts SET commission_source = ? WHERE id = ? AND user_id = ?;

-- name: ListAccounts :many
SELECT * FROM accounts WHERE user_id = ? ORDER BY name;

-- name: GetAccountByID :one
-- Returns sql.ErrNoRows if the user doesn't own this account, which the
-- caller can treat as 404 - this is the primary ownership gate for any
-- handler that takes an account id from the URL.
SELECT * FROM accounts WHERE id = ? AND user_id = ?;

-- name: UpdateAccount :one
UPDATE accounts
SET name = ?, firm = ?, type = ?, currency = ?, active = ?, needs_setup = ?
WHERE id = ? AND user_id = ?
RETURNING *;

-- name: ListRetiredAccountIDs :many
-- IDs of retired accounts (active = 0) belonging to the user. Used by
-- handlers that need a quick set lookup to skip retired-account rows in
-- already-fetched daily_stats or accounts lists.
SELECT id FROM accounts WHERE user_id = ? AND active = 0;

-- name: ListStatsExcludedAccountIDs :many
-- IDs of the user's accounts flagged excluded_from_stats. Handlers use the set
-- to drop those accounts from Dashboard/Calendar/Analytics default aggregates.
SELECT id FROM accounts WHERE user_id = ? AND excluded_from_stats = 1;

-- name: SetAccountExcluded :exec
-- Toggle an account's excluded_from_stats flag (Settings account-visibility
-- selector). Scoped by user so a guessed id can't flip another user's account.
UPDATE accounts SET excluded_from_stats = ? WHERE id = ? AND user_id = ?;

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
WHERE a.id = ? AND a.user_id = ?;

-- name: SetAccountAPIBackfillDone :exec
-- Marks that the TopstepX API full-history walk-back completed for the
-- account (0 to force a re-walk, used by the manual re-fetch button).
UPDATE accounts SET api_backfill_done = ? WHERE id = ? AND user_id = ?;

-- name: ListAccountsByRefAnyUser :many
-- Cross-user resolution for the MW email ingest. A fill-notification email
-- carries only the broker account ref - no user identity - so ownership is
-- derived from which user already has the account. A real broker ref belongs
-- to exactly one person; the caller treats 0 rows as "unknown account, do
-- not ingest" and 2+ rows as ambiguous (skip and warn).
SELECT * FROM accounts WHERE import_source = ? AND account_ref = ?;
