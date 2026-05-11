-- migrate:up
-- Switch commission_rates from (firm, symbol_root) to (asset_class, firm, size_class).
-- The pre-check in internal/db/migrations.go has already verified that every
-- row in the old table joins to an instrument with non-NULL asset_class and
-- size_class; if two symbols in the same (asset_class, size_class) disagree
-- on rate for the same firm, the INSERT below trips the PK constraint and
-- aborts the migration.
CREATE TABLE commission_rates_new (
    asset_class     TEXT NOT NULL REFERENCES asset_classes(code),
    firm            TEXT NOT NULL,
    size_class      TEXT NOT NULL REFERENCES size_classes(code),
    per_contract_rt REAL NOT NULL,
    PRIMARY KEY (asset_class, firm, size_class)
);

INSERT INTO commission_rates_new (asset_class, firm, size_class, per_contract_rt)
SELECT DISTINCT i.asset_class, c.firm, i.size_class, c.per_contract_rt
FROM commission_rates c
JOIN instruments i ON c.symbol_root = i.symbol_root
WHERE i.asset_class IS NOT NULL AND i.size_class IS NOT NULL;

DROP TABLE commission_rates;
ALTER TABLE commission_rates_new RENAME TO commission_rates;

-- migrate:down
-- No down-migration: translating back would require choosing which symbol
-- gets each rate, which is lossy. Restore from a snapshot if needed.
