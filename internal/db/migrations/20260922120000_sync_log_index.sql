-- migrate:up

-- The "newest row per sync type" (dashboard) and "newest row per account"
-- (settings) lookups used correlated subqueries that rescanned the whole
-- per-user history once per row: quadratic, 4 seconds on a 115k-row table.
-- The rewritten queries take MAX(id) per group; this index makes those group
-- scans index-only and also serves the retention purge's group lookups.
CREATE INDEX idx_sync_log_user_type_account_id ON sync_log(user_id, sync_type, account_id, id);

-- migrate:down
DROP INDEX idx_sync_log_user_type_account_id;
