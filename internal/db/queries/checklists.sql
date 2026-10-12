-- Playbook checklists (strategy items + per-entry ticks). ASCII-only
-- comments: sqlc miscounts multi-byte bytes and corrupts the next query.

-- name: ListChecklistItems :many
SELECT id, strategy_id, position, text FROM strategy_checklist_items
WHERE strategy_id = ? ORDER BY position, id;

-- name: ListChecklistItemsForUser :many
-- Every item of the user's strategies, for the editors (one block per strategy).
SELECT i.id, i.strategy_id, i.position, i.text
FROM strategy_checklist_items i JOIN strategies s ON s.id = i.strategy_id
WHERE s.user_id = ? ORDER BY i.strategy_id, i.position, i.id;

-- name: InsertChecklistItem :one
INSERT INTO strategy_checklist_items (strategy_id, position, text) VALUES (?, ?, ?) RETURNING id;

-- name: UpdateChecklistItemPosition :exec
UPDATE strategy_checklist_items SET position = ? WHERE id = ?;

-- name: DeleteChecklistItem :exec
DELETE FROM strategy_checklist_items WHERE id = ?;

-- name: DeleteEntryChecks :exec
DELETE FROM journal_entry_checks WHERE journal_entry_id = ?;

-- name: InsertEntryCheck :exec
INSERT OR IGNORE INTO journal_entry_checks (journal_entry_id, item_id) VALUES (?, ?);

-- name: ListEntryChecks :many
SELECT item_id FROM journal_entry_checks WHERE journal_entry_id = ?;
