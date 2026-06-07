-- migrate:up

-- price_bars is a PURE CACHE of historical OHLCV candles fetched from an
-- external market-data source (Yahoo v8 in v1). It is never source of truth and
-- can be dropped and refetched at any time. bar_time is unix SECONDS UTC
-- (INTEGER, not DATETIME) to avoid modernc string-coercion on range scans and to
-- match the unix seconds the front-end chart library consumes. The same
-- yahoo_symbol (e.g. ES=F) backs both the full and micro instrument roots.
CREATE TABLE price_bars (
    yahoo_symbol TEXT    NOT NULL,
    timeframe    TEXT    NOT NULL,   -- finest stored tf for the day: 1m|5m|1h|1d
    bar_time     INTEGER NOT NULL,   -- unix seconds, bar OPEN time, UTC
    open         REAL    NOT NULL,
    high         REAL    NOT NULL,
    low          REAL    NOT NULL,
    close        REAL    NOT NULL,
    volume       REAL    NOT NULL DEFAULT 0,
    fetched_at   INTEGER NOT NULL,   -- unix seconds; for debug / future TTL
    PRIMARY KEY (yahoo_symbol, timeframe, bar_time)
) WITHOUT ROWID;

-- price_bar_days records which (symbol, tf, exchange-tz trade day) windows have
-- been fetched, so a session full of legitimate gaps (weekend / holiday) is not
-- refetched on every view. Only COMPLETED past days are inserted; today and
-- future days are never marked so an in-progress session stays refetchable.
CREATE TABLE price_bar_days (
    yahoo_symbol TEXT    NOT NULL,
    timeframe    TEXT    NOT NULL,
    day          TEXT    NOT NULL,   -- 'YYYY-MM-DD' exchange-tz trade date
    fetched_at   INTEGER NOT NULL,
    PRIMARY KEY (yahoo_symbol, timeframe, day)
) WITHOUT ROWID;

-- migrate:down
DROP TABLE IF EXISTS price_bar_days;
DROP TABLE IF EXISTS price_bars;
