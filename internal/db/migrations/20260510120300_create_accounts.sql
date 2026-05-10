-- migrate:up
CREATE TABLE accounts (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL,
    firm          TEXT,           -- apex|lucid|topstep|amp|ibkr; NULL until user confirms
    import_source TEXT NOT NULL,  -- motivewave_csv|topstepx_csv
    account_ref   TEXT NOT NULL,  -- raw account string from CSV
    type          TEXT,           -- prop|live|paper; NULL until user sets
    currency      TEXT NOT NULL DEFAULT 'USD',
    needs_setup   INTEGER NOT NULL DEFAULT 0,
    active        INTEGER NOT NULL DEFAULT 1,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (import_source, account_ref)
);

-- migrate:down
DROP TABLE accounts;
