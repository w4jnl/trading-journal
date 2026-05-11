-- migrate:up
-- TopstepX's trades CSV now carries Fees + Commissions inline per trade, so
-- we don't need a rate-table lookup for topstep accounts anymore. Other
-- firms (apex/lucid/amp/ibkr) keep their seeded rates — those imports still
-- come through MotiveWave fills with no commission info.
DELETE FROM commission_rates WHERE firm = 'topstep';

-- migrate:down
INSERT INTO commission_rates (firm, symbol_root, per_contract_rt) VALUES
    ('topstep', 'ES',  1.90),
    ('topstep', 'NQ',  1.90),
    ('topstep', 'MES', 0.62),
    ('topstep', 'MNQ', 0.62),
    ('topstep', 'CL',  2.02),
    ('topstep', 'MCL', 0.77);
