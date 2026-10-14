-- migrate:up
-- Copy trades across accounts.
--
-- accounts.copy_group: accounts that a copier drives together (one name per
-- group, free text, NULL for an account that copies nothing). Legs on
-- accounts of the same group cluster into one set within a wider window
-- than the usual two seconds (internal/copyset): the copier adds latency,
-- IBKR reports fills to the second, and broker clocks differ.
--
-- position_plans.set_plan: 1 when the plan was set for the whole decision
-- (every leg of the set, including legs that arrive later), 0 when the
-- trader saved it for this leg only. A leg that joins a set without a plan
-- of its own inherits a set plan (source 'set'): the price on a stop order
-- the copier mirrored, the same target and thesis, the risk from its own
-- entry and size.
ALTER TABLE accounts ADD COLUMN copy_group TEXT;
ALTER TABLE position_plans ADD COLUMN set_plan INTEGER NOT NULL DEFAULT 0;

-- migrate:down
ALTER TABLE position_plans DROP COLUMN set_plan;
ALTER TABLE accounts DROP COLUMN copy_group;
