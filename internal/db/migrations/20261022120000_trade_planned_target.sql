-- migrate:up
-- The plan's profit target, next to planned_stop / planned_risk: a price on
-- the winning side of the entry, set in the trade sheet's plan row or from
-- the AI's proposed levels. Like the stop, it is the trader's and rides
-- along a FIFO rebuild (positions.Builder, by entry time).
ALTER TABLE trades ADD COLUMN planned_target REAL;

-- migrate:down
ALTER TABLE trades DROP COLUMN planned_target;
