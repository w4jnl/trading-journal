-- migrate:up
CREATE TABLE trade_executions (
    trade_id     INTEGER NOT NULL REFERENCES trades(id) ON DELETE CASCADE,
    execution_id INTEGER NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    role         TEXT NOT NULL,            -- entry|exit
    qty_used     REAL NOT NULL,            -- portion of the execution's qty consumed by this trade
    PRIMARY KEY (trade_id, execution_id, role)
);
CREATE INDEX idx_trade_executions_execution ON trade_executions(execution_id);

-- migrate:down
DROP INDEX idx_trade_executions_execution;
DROP TABLE trade_executions;
