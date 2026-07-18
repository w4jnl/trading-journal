-- migrate:up
-- Add a stable integer id to commission_rates so a version can be deleted by id
-- rather than by matching effective_from. A DATETIME text-equality match on the
-- 1970 baseline literal (written by 20260718120000) against a driver-bound
-- time.Time is fragile, so the delete path uses id instead. This is a separate
-- migration (not an edit to 20260718120000) because that migration was already
-- applied to a live DB; changing an applied migration would drift schemas.
-- effective_from values are carried over unchanged (unlike the prior rebuild,
-- which backfilled 1970), so any versions added between the two migrations are
-- preserved. UNIQUE keeps one rate per (cell, effective_from) and is the upsert
-- conflict target.
CREATE TABLE commission_rates_new (
    id             INTEGER PRIMARY KEY,
    asset_class    TEXT NOT NULL REFERENCES asset_classes(code),
    firm           TEXT NOT NULL,
    size_class     TEXT NOT NULL REFERENCES size_classes(code),
    effective_from DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
    per_contract_per_side REAL NOT NULL,
    UNIQUE (asset_class, firm, size_class, effective_from)
);

INSERT INTO commission_rates_new (asset_class, firm, size_class, effective_from, per_contract_per_side)
SELECT asset_class, firm, size_class, effective_from, per_contract_per_side
FROM commission_rates;

DROP TABLE commission_rates;
ALTER TABLE commission_rates_new RENAME TO commission_rates;

-- migrate:down
-- Drop id, restoring the (asset_class, firm, size_class, effective_from) PK.
CREATE TABLE commission_rates_old (
    asset_class    TEXT NOT NULL REFERENCES asset_classes(code),
    firm           TEXT NOT NULL,
    size_class     TEXT NOT NULL REFERENCES size_classes(code),
    effective_from DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
    per_contract_per_side REAL NOT NULL,
    PRIMARY KEY (asset_class, firm, size_class, effective_from)
);

INSERT INTO commission_rates_old (asset_class, firm, size_class, effective_from, per_contract_per_side)
SELECT asset_class, firm, size_class, effective_from, per_contract_per_side
FROM commission_rates;

DROP TABLE commission_rates;
ALTER TABLE commission_rates_old RENAME TO commission_rates;
