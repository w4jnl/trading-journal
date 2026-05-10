-- migrate:up
CREATE TABLE instrument_aliases (
    broker        TEXT NOT NULL,
    broker_symbol TEXT NOT NULL,
    instrument_id TEXT NOT NULL REFERENCES instruments(symbol_root),
    PRIMARY KEY (broker, broker_symbol)
);

-- migrate:down
DROP TABLE instrument_aliases;
