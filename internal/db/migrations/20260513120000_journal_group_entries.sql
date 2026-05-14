-- migrate:up
CREATE TABLE journal_entry_trades (
    journal_entry_id INTEGER NOT NULL REFERENCES journal_entries(id) ON DELETE CASCADE,
    trade_id         INTEGER NOT NULL REFERENCES trades(id)          ON DELETE CASCADE,
    PRIMARY KEY (journal_entry_id, trade_id)
);
CREATE INDEX idx_jet_trade ON journal_entry_trades(trade_id);

-- migrate:down
DROP INDEX IF EXISTS idx_jet_trade;
DROP TABLE IF EXISTS journal_entry_trades;
