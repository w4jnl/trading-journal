-- migrate:up
-- Copy sets: one decision fanned across accounts by a copytrader. A set is
-- derived from closed trades (same underlying, same direction, entries
-- within two seconds, two or more accounts) and rebuilt from scratch after
-- every trade rebuild or import, like daily_stats. Trades outside any set
-- are their own unit. Ids are not stable across rebuilds; nothing links to
-- a set by id (journal entries link to the member trades).
CREATE TABLE trade_sets (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    cluster_key  TEXT     NOT NULL,           -- underlying key (ES=F for ES and MES)
    symbol_root  TEXT     NOT NULL,           -- root of the first member, for display
    direction    TEXT     NOT NULL,           -- long|short
    entry_time   DATETIME NOT NULL,           -- earliest member entry, UTC
    exit_time    DATETIME,                    -- latest member exit, UTC
    members      INTEGER  NOT NULL,
    accounts     INTEGER  NOT NULL,
    qty          REAL     NOT NULL,           -- contracts across members
    entry_avg    REAL     NOT NULL,
    entry_spread REAL     NOT NULL,           -- max minus min entry price across members
    exit_avg     REAL,
    gross_pnl    REAL,
    commission   REAL,
    net_pnl      REAL
);
CREATE INDEX idx_trade_sets_user_entry ON trade_sets(user_id, entry_time);

CREATE TABLE trade_set_members (
    trade_id INTEGER PRIMARY KEY REFERENCES trades(id) ON DELETE CASCADE,
    set_id   INTEGER NOT NULL REFERENCES trade_sets(id) ON DELETE CASCADE
);
CREATE INDEX idx_trade_set_members_set ON trade_set_members(set_id);

-- migrate:down
DROP TABLE trade_set_members;
DROP TABLE trade_sets;
