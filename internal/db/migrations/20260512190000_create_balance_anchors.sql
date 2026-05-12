-- migrate:up
CREATE TABLE balance_anchors (
    id          INTEGER PRIMARY KEY,
    account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    anchor_date DATE NOT NULL,
    balance     REAL NOT NULL,
    note        TEXT,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (account_id, anchor_date)
);
CREATE INDEX idx_balance_anchors_account_date ON balance_anchors(account_id, anchor_date);

-- migrate:down
DROP INDEX IF EXISTS idx_balance_anchors_account_date;
DROP TABLE IF EXISTS balance_anchors;
