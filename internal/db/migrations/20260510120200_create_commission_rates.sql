-- migrate:up
CREATE TABLE commission_rates (
    firm            TEXT NOT NULL,
    symbol_root     TEXT NOT NULL REFERENCES instruments(symbol_root),
    per_contract_rt REAL NOT NULL,
    PRIMARY KEY (firm, symbol_root)
);

-- migrate:down
DROP TABLE commission_rates;
