-- name: GetDayJournalEntry :one
SELECT * FROM journal_entries
WHERE entry_date = ? AND trade_id IS NULL
LIMIT 1;

-- name: CreateJournalEntry :one
INSERT INTO journal_entries (trade_id, account_id, entry_date, body, mood)
VALUES (?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateJournalEntry :exec
UPDATE journal_entries
SET body = ?, mood = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ?;

-- name: DeleteJournalEntry :exec
DELETE FROM journal_entries WHERE id = ?;

-- name: ListRecentJournalEntries :many
SELECT * FROM journal_entries
WHERE trade_id IS NULL
ORDER BY entry_date DESC, id DESC
LIMIT ?;
