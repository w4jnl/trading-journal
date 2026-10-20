-- migrate:up
-- The day map (internal/levelmap). level_feeds is what a MotiveWave chart
-- posted (docs/api-handoff-002-levels-feed.md): every accepted post, kept
-- for a month; the newest as_of per user, market and trading day is the
-- map's chart overlay. level_maps is the built map as JSON, one row per
-- user, market, trading day and phase: premarket (frozen once the RTH
-- open passed) and session (the latest), so a past day opens as it stood.
CREATE TABLE level_feeds (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    market      TEXT     NOT NULL,
    trade_day   TEXT     NOT NULL,
    as_of       DATETIME NOT NULL,
    received_at DATETIME NOT NULL,
    contract    TEXT     NOT NULL,
    payload     TEXT     NOT NULL
);
CREATE INDEX idx_level_feeds_user_day ON level_feeds(user_id, market, trade_day, as_of);

CREATE TABLE level_maps (
    user_id    INTEGER  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    market     TEXT     NOT NULL,
    trade_day  TEXT     NOT NULL,
    phase      TEXT     NOT NULL,
    as_of      DATETIME NOT NULL,
    data       TEXT     NOT NULL,
    PRIMARY KEY (user_id, market, trade_day, phase)
);

-- migrate:down
DROP TABLE level_maps;
DROP TABLE level_feeds;
