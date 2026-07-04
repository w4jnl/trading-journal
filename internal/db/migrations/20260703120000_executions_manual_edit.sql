-- migrate:up
-- Support manual reconciliation of executions (the /admin/transactions editor).
--   deleted_at: soft-delete tombstone. The FIFO builder ignores rows where this is
--     set. Crucially the row is RETAINED so the importer's
--     ON CONFLICT(account_id, broker_exec_id) DO NOTHING can't resurrect a deleted
--     fill on the next re-import.
--   manual: 1 for hand-created rows (broker_exec_id is a synthetic MANUAL-... id).
--   edited_at: last hand-edit, for a light audit trail.
ALTER TABLE executions ADD COLUMN deleted_at DATETIME;
ALTER TABLE executions ADD COLUMN manual     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE executions ADD COLUMN edited_at  DATETIME;

-- migrate:down
ALTER TABLE executions DROP COLUMN edited_at;
ALTER TABLE executions DROP COLUMN manual;
ALTER TABLE executions DROP COLUMN deleted_at;
