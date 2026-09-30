-- migrate:up

-- Trade-level delete in the reconcile editor (/admin/transactions?view=trades)
-- tombstones every fill of one trade in a single action. deleted_group ties
-- those fills together so the set can be restored as a unit later: a trade id
-- is not a usable handle because every FIFO rebuild assigns fresh ids.
-- NULL for live rows and for fills deleted one by one in the fills view.
-- Cleared together with deleted_at on restore (per fill or per group).
ALTER TABLE executions ADD COLUMN deleted_group TEXT;
CREATE INDEX idx_executions_deleted_group ON executions(deleted_group) WHERE deleted_group IS NOT NULL;

-- migrate:down
DROP INDEX idx_executions_deleted_group;
ALTER TABLE executions DROP COLUMN deleted_group;
