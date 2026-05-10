-- migrate:up
CREATE TABLE trades (
    id           INTEGER PRIMARY KEY,
    account_id   INTEGER NOT NULL REFERENCES accounts(id),
    symbol       TEXT NOT NULL,
    symbol_root  TEXT NOT NULL REFERENCES instruments(symbol_root),
    direction    TEXT NOT NULL,                  -- long|short
    qty          REAL NOT NULL,                  -- max contracts held during the trade
    entry_price  REAL NOT NULL,                  -- volume-weighted average entry
    exit_price   REAL,                           -- volume-weighted average exit; NULL while open
    entry_time   DATETIME NOT NULL,              -- UTC, first opening fill
    exit_time    DATETIME,                       -- UTC, last closing fill; NULL while open
    gross_pnl    REAL,
    commission   REAL,
    net_pnl      REAL,
    status       TEXT NOT NULL DEFAULT 'open',   -- open|closed
    tags         TEXT,                           -- JSON array of strings
    setup        TEXT,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_trades_account_entry ON trades(account_id, entry_time);
CREATE INDEX idx_trades_status ON trades(status);

-- migrate:down
DROP INDEX idx_trades_status;
DROP INDEX idx_trades_account_entry;
DROP TABLE trades;
