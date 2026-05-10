-- migrate:up
CREATE TABLE instruments (
    symbol_root TEXT PRIMARY KEY,
    exchange    TEXT,                            -- nullable: TopstepX export omits exchange; stub rows leave it NULL
    tick_size   REAL,                            -- nullable: stub rows have no tick spec until user configures
    tick_value  REAL,                            -- nullable: same as tick_size
    multiplier  REAL,                            -- nullable: same as tick_size
    currency    TEXT NOT NULL DEFAULT 'USD',
    needs_setup INTEGER NOT NULL DEFAULT 0       -- 1 = stub auto-created by importer, awaiting user input
);

-- migrate:down
DROP TABLE instruments;
