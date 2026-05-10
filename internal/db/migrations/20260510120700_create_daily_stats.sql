-- migrate:up
CREATE TABLE daily_stats (
    id           INTEGER PRIMARY KEY,
    account_id   INTEGER NOT NULL REFERENCES accounts(id),
    stat_date    DATE NOT NULL,
    trades_count INTEGER NOT NULL DEFAULT 0,
    wins         INTEGER NOT NULL DEFAULT 0,
    losses       INTEGER NOT NULL DEFAULT 0,
    gross_pnl    REAL NOT NULL DEFAULT 0,
    net_pnl      REAL NOT NULL DEFAULT 0,
    commission   REAL NOT NULL DEFAULT 0,
    largest_win  REAL,
    largest_loss REAL,
    UNIQUE (account_id, stat_date)
);

-- migrate:down
DROP TABLE daily_stats;
