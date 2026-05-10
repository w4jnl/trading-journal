-- migrate:up
CREATE TABLE executions (
    id             INTEGER PRIMARY KEY,
    account_id     INTEGER NOT NULL REFERENCES accounts(id),
    trade_id       INTEGER REFERENCES trades(id),
    broker_exec_id TEXT NOT NULL,
    symbol         TEXT NOT NULL,
    symbol_root    TEXT NOT NULL REFERENCES instruments(symbol_root),
    side           TEXT NOT NULL,            -- buy|sell
    qty            REAL NOT NULL,
    price          REAL NOT NULL,
    commission     REAL,
    exec_time      DATETIME NOT NULL,        -- UTC
    raw_data       TEXT,
    UNIQUE (account_id, broker_exec_id)
);
CREATE INDEX idx_executions_account_time ON executions(account_id, exec_time);
CREATE INDEX idx_executions_trade ON executions(trade_id);

-- migrate:down
DROP INDEX idx_executions_trade;
DROP INDEX idx_executions_account_time;
DROP TABLE executions;
