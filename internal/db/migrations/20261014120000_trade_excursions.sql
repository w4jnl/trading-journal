-- migrate:up
-- Price excursion while the trade was open, read off cached bars: how far
-- price went in the trade's favour (MFE) and against it (MAE), in points
-- from the average entry and in money through the instrument's point
-- value. excursion_tf is the bar timeframe the numbers came from (1m, 5m,
-- 1h, 1d); NULL means not computed yet (no cached bars, or bars too coarse
-- for the holding time). Derived data: the builder carries it across a
-- rebuild only when the trade's boundaries did not move, and the filler
-- recomputes the rest from the bar cache.
ALTER TABLE trades ADD COLUMN mae_points REAL;
ALTER TABLE trades ADD COLUMN mfe_points REAL;
ALTER TABLE trades ADD COLUMN mae_pnl REAL;
ALTER TABLE trades ADD COLUMN mfe_pnl REAL;
ALTER TABLE trades ADD COLUMN excursion_tf TEXT;

-- migrate:down
ALTER TABLE trades DROP COLUMN excursion_tf;
ALTER TABLE trades DROP COLUMN mfe_pnl;
ALTER TABLE trades DROP COLUMN mae_pnl;
ALTER TABLE trades DROP COLUMN mfe_points;
ALTER TABLE trades DROP COLUMN mae_points;
