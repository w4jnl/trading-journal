-- name: ListEntryLabels :many
SELECT kind, label, source FROM journal_entry_labels
WHERE journal_entry_id = ?
ORDER BY kind, label;

-- name: DeleteEntryLabelsOfKind :exec
DELETE FROM journal_entry_labels WHERE journal_entry_id = ? AND kind = ?;

-- name: InsertEntryLabel :exec
-- source: 'trader' (a page editor or an accepted suggestion) or 'api'. A
-- label already present keeps its row (and source).
INSERT OR IGNORE INTO journal_entry_labels (journal_entry_id, kind, label, source) VALUES (?, ?, ?, ?);

-- name: MarkEntrySourcesTrader :exec
-- A page save: whatever tag and strategy the entry now holds are the trader's.
UPDATE journal_entries
SET tag_source = CASE WHEN tag IS NULL THEN NULL ELSE 'trader' END,
    strategy_source = CASE WHEN strategy_id IS NULL THEN NULL ELSE 'trader' END
WHERE id = ?;

-- name: SetEntryTagSource :exec
UPDATE journal_entries SET tag_source = ? WHERE id = ?;

-- name: SetEntryStrategySource :exec
UPDATE journal_entries SET strategy_source = ? WHERE id = ?;

-- name: UpsertSuggestion :exec
-- A dismissed suggestion stays dismissed when a later review proposes it again.
INSERT INTO journal_suggestions (journal_entry_id, kind, value) VALUES (?, ?, ?)
ON CONFLICT (journal_entry_id, kind, value) DO NOTHING;

-- name: ListOpenSuggestions :many
SELECT kind, value FROM journal_suggestions
WHERE journal_entry_id = ? AND status = 'open'
ORDER BY CASE kind WHEN 'tag' THEN 0 WHEN 'strategy' THEN 1 WHEN 'mistake' THEN 2 ELSE 3 END, value;

-- name: GetSuggestion :one
SELECT kind, value, status FROM journal_suggestions
WHERE journal_entry_id = ? AND kind = ? AND value = ?;

-- name: DeleteSuggestion :exec
DELETE FROM journal_suggestions WHERE journal_entry_id = ? AND kind = ? AND value = ?;

-- name: DismissSuggestion :exec
UPDATE journal_suggestions SET status = 'dismissed'
WHERE journal_entry_id = ? AND kind = ? AND value = ?;

-- name: DropOtherOpenSuggestions :exec
-- A tag or strategy takes one value: a newer review's proposal replaces the
-- open one before it (a dismissed one stays, so it is not proposed again).
DELETE FROM journal_suggestions
WHERE journal_entry_id = ? AND kind = ? AND status = 'open' AND value != ?;
