-- Queries against the balance_anchors table, used by the /accounts page and
-- the per-trade running-balance column on /trades. The trade-side delta walks
-- live in trades.sql (ListClosedTradeDeltasOrdered).

-- name: ListBalanceAnchors :many
-- Every anchor, ordered so the loadBalances pass can walk per-account.
-- COALESCE on `note` keeps the consumer free of nullable-string handling
-- since the empty string is the natural default in the UI.
SELECT id, account_id, anchor_date, balance, COALESCE(note, '') AS note
FROM balance_anchors
ORDER BY account_id, anchor_date ASC, id ASC;

-- name: UpsertBalanceAnchor :exec
-- Same-day re-saves overwrite -- lets the user fix a typo without first
-- deleting the old anchor.
INSERT INTO balance_anchors (account_id, anchor_date, balance, note)
VALUES (?, ?, ?, ?)
ON CONFLICT (account_id, anchor_date) DO UPDATE SET
    balance = excluded.balance,
    note    = excluded.note;

-- name: DeleteBalanceAnchor :exec
DELETE FROM balance_anchors WHERE id = ?;
