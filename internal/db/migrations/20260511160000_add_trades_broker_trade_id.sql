-- migrate:up
-- TopstepX's trades export gives us pre-computed trades with a stable Id per
-- row. We persist that as broker_trade_id so re-imports dedup cleanly without
-- having to re-derive the trade from synthetic executions. Partial unique
-- index lets FIFO-built trades (broker_trade_id NULL) coexist alongside.
ALTER TABLE trades ADD COLUMN broker_trade_id TEXT;
CREATE UNIQUE INDEX idx_trades_broker_id ON trades(account_id, broker_trade_id)
WHERE broker_trade_id IS NOT NULL;

-- migrate:down
DROP INDEX idx_trades_broker_id;
ALTER TABLE trades DROP COLUMN broker_trade_id;
