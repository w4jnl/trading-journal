-- name: GetDayJournalEntry :one
SELECT * FROM journal_entries
WHERE entry_date = ? AND trade_id IS NULL
LIMIT 1;

-- name: GetJournalEntryByTradeID :one
SELECT * FROM journal_entries WHERE trade_id = ? LIMIT 1;

-- name: CreateJournalEntry :one
INSERT INTO journal_entries (trade_id, account_id, entry_date, body, mood, tag)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateJournalEntry :exec
UPDATE journal_entries
SET body = ?, mood = ?, tag = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ?;

-- name: DeleteJournalEntry :exec
DELETE FROM journal_entries WHERE id = ?;

-- name: ListRecentJournalEntries :many
SELECT * FROM journal_entries
WHERE trade_id IS NULL
ORDER BY entry_date DESC, id DESC
LIMIT ?;

-- name: ListDayJournalsInRange :many
-- Used by /calendar to flag which dates have day-level entries.
SELECT entry_date, tag FROM journal_entries
WHERE trade_id IS NULL AND entry_date >= ? AND entry_date < ?
ORDER BY entry_date;
