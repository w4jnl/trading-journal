-- migrate:up
-- idx_trades_status has zero selectivity (every row is 'closed' once the
-- position is flat) yet the planner picked it for the trades list, which
-- then scanned and sorted in a temp B-tree. The hot filters are per account
-- by exit time (dashboard today/windows, journal day lists, analytics,
-- reconcile), which (account_id, exit_time) serves directly; the existing
-- (account_id, entry_time) index stays for the FIFO-order and relink paths.
DROP INDEX IF EXISTS idx_trades_status;
CREATE INDEX IF NOT EXISTS idx_trades_account_exit ON trades(account_id, exit_time);

-- migrate:down
DROP INDEX IF EXISTS idx_trades_account_exit;
CREATE INDEX IF NOT EXISTS idx_trades_status ON trades(status);
