-- migrate:up
-- Make commission rates time-versioned: a (asset_class, firm, size_class) cell
-- can now hold multiple rows, each with an effective_from datetime (UTC). A
-- trade's fees use the rate in effect at each fill's exec_time (per side). SQLite
-- can't add a column to an existing PRIMARY KEY in place, so rebuild the table
-- (precedent: 20260512100200_rebuild_commission_rates.sql). Every existing rate
-- is backfilled with effective_from = 1970-01-01 so it still covers all historical
-- trades - commissions are unchanged until the user adds a later version.
CREATE TABLE commission_rates_new (
    asset_class    TEXT NOT NULL REFERENCES asset_classes(code),
    firm           TEXT NOT NULL,
    size_class     TEXT NOT NULL REFERENCES size_classes(code),
    effective_from DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
    per_contract_per_side REAL NOT NULL,
    PRIMARY KEY (asset_class, firm, size_class, effective_from)
);

INSERT INTO commission_rates_new (asset_class, firm, size_class, effective_from, per_contract_per_side)
SELECT asset_class, firm, size_class, '1970-01-01 00:00:00', per_contract_per_side
FROM commission_rates;

DROP TABLE commission_rates;
ALTER TABLE commission_rates_new RENAME TO commission_rates;

-- migrate:down
-- Lossy: collapses each cell to its latest version (the current rate).
CREATE TABLE commission_rates_old (
    asset_class     TEXT NOT NULL REFERENCES asset_classes(code),
    firm            TEXT NOT NULL,
    size_class      TEXT NOT NULL REFERENCES size_classes(code),
    per_contract_per_side REAL NOT NULL,
    PRIMARY KEY (asset_class, firm, size_class)
);

INSERT INTO commission_rates_old (asset_class, firm, size_class, per_contract_per_side)
SELECT c.asset_class, c.firm, c.size_class, c.per_contract_per_side
FROM commission_rates c
WHERE c.effective_from = (
    SELECT MAX(c2.effective_from) FROM commission_rates c2
    WHERE c2.asset_class = c.asset_class AND c2.firm = c.firm AND c2.size_class = c.size_class
);

DROP TABLE commission_rates;
ALTER TABLE commission_rates_old RENAME TO commission_rates;
