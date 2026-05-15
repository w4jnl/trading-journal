-- migrate:up
-- TopstepX orders export has no commission column, so accounts imported from
-- it need a commission_rates lookup like every other firm. (TopstepX trades
-- export carries commissions inline; those accounts have commission_source =
-- 'inline' from migration 20260515120000 and bypass this lookup entirely.)
--
-- Reseeds the rates that were dropped in 20260511160100, translated to the
-- (asset_class, firm, size_class) shape introduced in 20260512100200.
INSERT OR IGNORE INTO commission_rates (asset_class, firm, size_class, per_contract_rt) VALUES
    ('equity_futures', 'topstep', 'mini',     1.90),
    ('equity_futures', 'topstep', 'micro',    0.62),
    ('energy_futures', 'topstep', 'standard', 2.02),
    ('energy_futures', 'topstep', 'micro',    0.77);

-- migrate:down
DELETE FROM commission_rates WHERE firm = 'topstep';
