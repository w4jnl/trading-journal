-- Queries against the balance_anchors table, used by the /accounts page and
-- the per-trade running-balance column on /trades. The trade-side delta walks
-- live in trades.sql (ListClosedTradeDeltasOrdered).

-- name: ListBalanceAnchors :many
-- Every anchor for this user, ordered so the loadBalances pass can walk
-- per-account. COALESCE on `note` keeps the consumer free of nullable-string
-- handling since the empty string is the natural default in the UI.
SELECT ba.id, ba.account_id, ba.anchor_date, ba.balance, COALESCE(ba.note, '') AS note
FROM balance_anchors ba
JOIN accounts a ON a.id = ba.account_id
WHERE a.user_id = ?
ORDER BY ba.account_id, ba.anchor_date ASC, ba.id ASC;

-- name: UpsertBalanceAnchor :exec
-- Same-day re-saves overwrite -- lets the user fix a typo without first
-- deleting the old anchor. Caller must have verified account ownership.
INSERT INTO balance_anchors (account_id, anchor_date, balance, note)
VALUES (?, ?, ?, ?)
ON CONFLICT (account_id, anchor_date) DO UPDATE SET
    balance = excluded.balance,
    note    = excluded.note;

-- name: DeleteBalanceAnchor :exec
-- Caller must have verified the parent account belongs to the user. The
-- subquery enforces this defensively in case a handler forgets to check.
DELETE FROM balance_anchors
WHERE balance_anchors.id = ?
  AND balance_anchors.account_id IN (SELECT accounts.id FROM accounts WHERE accounts.user_id = ?);
