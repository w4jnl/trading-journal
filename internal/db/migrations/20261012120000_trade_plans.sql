-- migrate:up
-- Planned risk per trade (true R). planned_stop is the stop price the
-- trader intended at entry; planned_risk the money at risk it implies in
-- the account currency (derived from the instrument's tick specs, or
-- entered directly). R multiple = net_pnl / planned_risk. The position
-- builder carries both across rebuilds keyed on the opening fill, the way
-- it keeps journal links. NULL = no plan recorded.
ALTER TABLE trades ADD COLUMN planned_stop REAL;
ALTER TABLE trades ADD COLUMN planned_risk REAL;

-- migrate:down
ALTER TABLE trades DROP COLUMN planned_risk;
ALTER TABLE trades DROP COLUMN planned_stop;
