-- migrate:up
-- The junction table trade_executions now models trade↔fill participation
-- (handles position flips where a single fill belongs to two trades).
-- The single FK on executions can't represent that, so drop it.
DROP INDEX idx_executions_trade;
ALTER TABLE executions DROP COLUMN trade_id;

-- migrate:down
ALTER TABLE executions ADD COLUMN trade_id INTEGER REFERENCES trades(id);
CREATE INDEX idx_executions_trade ON executions(trade_id);
