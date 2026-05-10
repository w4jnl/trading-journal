-- migrate:up
CREATE TABLE sync_log (
    id           INTEGER PRIMARY KEY,
    account_id   INTEGER REFERENCES accounts(id), -- NULL when import touched many accounts (MotiveWave) or none (parse failure)
    sync_type    TEXT NOT NULL,                   -- motivewave_csv|topstepx_csv
    status       TEXT NOT NULL,                   -- success|error|partial
    source_file  TEXT,                            -- original filename (post-move path stored as info)
    trades_added INTEGER NOT NULL DEFAULT 0,
    execs_added  INTEGER NOT NULL DEFAULT 0,
    message      TEXT,
    synced_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_sync_log_synced_at ON sync_log(synced_at DESC);

-- migrate:down
DROP INDEX idx_sync_log_synced_at;
DROP TABLE sync_log;
