-- migrate:up
-- Pre-market plan: one per user and trading day (day is YYYY-MM-DD in the
-- trading timezone), written before the session and read against the
-- day's trades afterwards (internal/coach). bias is long, short, neutral
-- (both ways) or flat (stay out), empty for none; max_trades counts
-- decisions (a copy set once); stop_time is HH:MM in the trading timezone,
-- empty for none; max_loss is per account (the worst account's running net
-- is compared with it). created_at / updated_at show whether the plan was
-- written before the first trade.
CREATE TABLE day_plans (
    user_id    INTEGER  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day        TEXT     NOT NULL,
    bias       TEXT     NOT NULL DEFAULT '',
    levels     TEXT     NOT NULL DEFAULT '',
    max_trades INTEGER,
    stop_time  TEXT     NOT NULL DEFAULT '',
    max_loss   REAL,
    notes      TEXT     NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (user_id, day)
);

-- migrate:down
DROP TABLE day_plans;
