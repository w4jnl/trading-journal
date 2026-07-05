-- name: GetDayJournalEntry :one
SELECT * FROM journal_entries
WHERE user_id = ? AND entry_date = ? AND scope = 'day'
LIMIT 1;

-- name: GetWeekJournalEntry :one
-- entry_date for week-scoped rows is the Monday of that ISO week.
SELECT * FROM journal_entries
WHERE user_id = ? AND entry_date = ? AND scope = 'week'
LIMIT 1;

-- name: GetJournalEntryByTradeID :one
-- The trade itself is already user-owned (verified via GetTradeByID), so
-- this query trusts the trade_id and doesn't re-check user_id.
SELECT * FROM journal_entries WHERE trade_id = ? LIMIT 1;

-- name: CreateJournalEntry :one
INSERT INTO journal_entries (user_id, trade_id, account_id, entry_date, body, mood, tag, scope, strategy_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateJournalEntry :exec
-- Scope by user_id so a guessed entry id can't update someone else's entry.
UPDATE journal_entries
SET body = ?, mood = ?, tag = ?, strategy_id = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;

-- name: DeleteJournalEntry :exec
DELETE FROM journal_entries WHERE id = ? AND user_id = ?;

-- name: ListRecentJournalEntries :many
SELECT * FROM journal_entries
WHERE user_id = ? AND scope = 'day'
ORDER BY entry_date DESC, id DESC
LIMIT ?;

-- name: ListRecentJournalEntriesForEditor :many
-- Backs the editor's "Recall" dropdown: recent entries of one scope, excluding
-- the entry currently being edited. Ordered newest-first. Scoped by owner.
SELECT id, entry_date, body, tag FROM journal_entries
WHERE scope = ? AND id != ? AND user_id = ?
ORDER BY entry_date DESC, id DESC
LIMIT ?;

-- name: ListDayJournalsInRange :many
-- Used by /calendar to flag which dates have day-level entries.
SELECT entry_date, tag FROM journal_entries
WHERE user_id = ? AND scope = 'day' AND entry_date >= ? AND entry_date < ?
ORDER BY entry_date;

-- name: ListWeekJournalsInRange :many
SELECT entry_date, tag FROM journal_entries
WHERE user_id = ? AND scope = 'week' AND entry_date >= ? AND entry_date < ?
ORDER BY entry_date;

-- name: ListJournalFeed :many
-- The cross-scope chronological feed. Joins in trade + account context so the
-- feed item for a trade-scoped entry can render the symbol/direction/account
-- in the row without a second query per entry.
SELECT je.id, je.trade_id, je.account_id, je.entry_date, je.body, je.mood, je.tag, je.scope,
       je.created_at, je.updated_at,
       t.symbol AS trade_symbol, t.symbol_root AS trade_symbol_root,
       t.direction AS trade_direction, t.net_pnl AS trade_net_pnl,
       a.name AS account_name
FROM journal_entries je
LEFT JOIN trades t   ON t.id = je.trade_id
LEFT JOIN accounts a ON a.id = COALESCE(je.account_id, t.account_id)
WHERE je.user_id = ?
ORDER BY je.entry_date DESC, je.id DESC
LIMIT ?;

-- name: GetJournalEntryByID :one
-- Fetch one entry scoped by owner. Added for the API (no by-id query existed).
SELECT * FROM journal_entries WHERE id = ? AND user_id = ?;

-- name: AppendJournalEntryBody :exec
-- Append-only note write for the API: concatenates onto the existing body in a
-- single atomic UPDATE so concurrent appends can't lose each other's text (a
-- Go-side read-modify-write could). The caller passes the fully formatted block
-- to append (separator + dated heading + note). Scoped by owner.
UPDATE journal_entries
SET body = body || ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ?;
