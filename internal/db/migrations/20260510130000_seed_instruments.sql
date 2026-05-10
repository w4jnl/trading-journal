-- migrate:up
INSERT INTO instruments (symbol_root, exchange, tick_size, tick_value, multiplier, currency) VALUES
    ('ES',  'CME',   0.25,  12.50, 50,    'USD'),
    ('MES', 'CME',   0.25,   1.25,  5,    'USD'),
    ('NQ',  'CME',   0.25,   5.00, 20,    'USD'),
    ('MNQ', 'CME',   0.25,   0.50,  2,    'USD'),
    ('CL',  'NYMEX', 0.01,  10.00, 1000,  'USD'),
    ('MCL', 'NYMEX', 0.01,   1.00, 100,   'USD');

-- migrate:down
DELETE FROM instruments WHERE symbol_root IN ('ES','MES','NQ','MNQ','CL','MCL');
