-- migrate:up
INSERT INTO commission_rates (firm, symbol_root, per_contract_rt) VALUES
    ('topstep', 'ES',  1.90),
    ('topstep', 'NQ',  1.90),
    ('topstep', 'MES', 0.62),
    ('topstep', 'MNQ', 0.62),
    ('topstep', 'CL',  2.02),
    ('topstep', 'MCL', 0.77),
    ('apex',    'ES',  1.99),
    ('apex',    'NQ',  1.99),
    ('apex',    'MES', 0.51),
    ('apex',    'MNQ', 0.51),
    ('apex',    'CL',  1.98),
    ('apex',    'MCL', 0.51),
    ('lucid',   'ES',  1.75),
    ('lucid',   'NQ',  1.75),
    ('lucid',   'MES', 0.50),
    ('lucid',   'MNQ', 0.50),
    ('lucid',   'CL',  2.00),
    ('lucid',   'MCL', 0.50),
    ('amp',     'ES',  2.25),
    ('amp',     'NQ',  2.25),
    ('amp',     'MES', 0.62),
    ('amp',     'MNQ', 0.62),
    ('ibkr',    'ES',  2.50),
    ('ibkr',    'NQ',  2.50),
    ('ibkr',    'MES', 0.75),
    ('ibkr',    'MNQ', 0.75),
    ('ibkr',    'CL',  2.50),
    ('ibkr',    'MCL', 0.75);

-- migrate:down
DELETE FROM commission_rates;
