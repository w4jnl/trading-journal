-- migrate:up
CREATE TABLE journal_entries (
    id         INTEGER PRIMARY KEY,
    trade_id   INTEGER REFERENCES trades(id),       -- NULL = day-level entry
    account_id INTEGER REFERENCES accounts(id),
    entry_date DATE NOT NULL,
    body       TEXT NOT NULL,
    mood       INTEGER,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_journal_entry_date ON journal_entries(entry_date);
CREATE INDEX idx_journal_trade ON journal_entries(trade_id);

-- migrate:down
DROP INDEX idx_journal_trade;
DROP INDEX idx_journal_entry_date;
DROP TABLE journal_entries;
