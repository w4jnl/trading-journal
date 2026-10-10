-- name: ListEntryLabels :many
SELECT kind, label FROM journal_entry_labels
WHERE journal_entry_id = ?
ORDER BY kind, label;

-- name: DeleteEntryLabelsOfKind :exec
DELETE FROM journal_entry_labels WHERE journal_entry_id = ? AND kind = ?;

-- name: InsertEntryLabel :exec
INSERT OR IGNORE INTO journal_entry_labels (journal_entry_id, kind, label) VALUES (?, ?, ?);
