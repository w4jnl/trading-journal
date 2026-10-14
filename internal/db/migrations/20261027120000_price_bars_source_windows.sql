-- migrate:up
-- Exchange bars in the shared cache.
--
-- price_bar_days.source: which feed filled a cached day: 'yahoo' (the
-- continuous front-month series) or 'topstepx' (the exact contract's
-- exchange data through a user's TopstepX session, used first for the
-- recent days of the roots being traded). The charts drop their basis note
-- for a day that came from the exchange.
--
-- price_bar_windows: the stretches fetched on demand at one second around
-- short trades (entry to exit, padded), stored in price_bars under
-- timeframe '1s' as a layer of their own: the day-level reads never roll
-- them in, the excursion filler and the chart's second sizes read them when
-- a window covers the trade. Pruned with the intraday bars.
ALTER TABLE price_bar_days ADD COLUMN source TEXT NOT NULL DEFAULT 'yahoo';

CREATE TABLE price_bar_windows (
    yahoo_symbol TEXT    NOT NULL,
    timeframe    TEXT    NOT NULL,
    from_time    INTEGER NOT NULL,   -- unix seconds, inclusive
    to_time      INTEGER NOT NULL,   -- unix seconds, exclusive
    source       TEXT    NOT NULL,
    fetched_at   INTEGER NOT NULL,
    PRIMARY KEY (yahoo_symbol, timeframe, from_time)
) WITHOUT ROWID;

-- migrate:down
DROP TABLE price_bar_windows;
ALTER TABLE price_bar_days DROP COLUMN source;
