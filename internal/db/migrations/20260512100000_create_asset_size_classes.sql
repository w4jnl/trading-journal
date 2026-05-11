-- migrate:up
CREATE TABLE asset_classes (
    code       TEXT PRIMARY KEY,
    label      TEXT NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE size_classes (
    code       TEXT PRIMARY KEY,
    label      TEXT NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0
);

INSERT INTO asset_classes (code, label, sort_order) VALUES
    ('equity_futures',        'Equity Index Futures', 10),
    ('energy_futures',        'Energy Futures',       20),
    ('metals_futures',        'Metals Futures',       30),
    ('interest_rate_futures', 'Interest Rate Futures',40),
    ('currency_futures',      'Currency Futures',     50),
    ('agricultural_futures',  'Agricultural Futures', 60),
    ('crypto_futures',        'Crypto Futures',       70);

INSERT INTO size_classes (code, label, sort_order) VALUES
    ('standard', 'Standard', 10),
    ('mini',     'Mini',     20),
    ('micro',    'Micro',    30);

-- migrate:down
DROP TABLE size_classes;
DROP TABLE asset_classes;
