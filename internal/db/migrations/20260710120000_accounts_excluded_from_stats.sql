-- migrate:up
-- Per-account "hide from analytical views" flag. When set, the account's trades
-- are excluded from the DEFAULT (unfiltered) aggregates on the Dashboard,
-- Calendar, and Analytics views - the common case being prop-firm Eval accounts
-- whose fake-money P&L would distort the real picture. The account stays fully
-- visible in Accounts management, still imports normally, and still shows on
-- /trades; an explicit account filter on Calendar/Analytics also overrides the
-- flag so the user can still drill into an excluded account on demand.
ALTER TABLE accounts ADD COLUMN excluded_from_stats INTEGER NOT NULL DEFAULT 0;

-- migrate:down
ALTER TABLE accounts DROP COLUMN excluded_from_stats;
