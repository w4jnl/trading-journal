-- migrate:up
-- The intraday guardrail: while the session runs, the server re-reads the
-- day as fills arrive (the coach's checks, the plan's cap, max loss and
-- stop time) and records a nudge the first time each one applies; the open
-- page shows it (and the browser, when the trader allowed notifications).
-- users.guardrail: the trader turned it on in Settings.
-- nudges: one row per thing said, unique on (user, day, key, ref) so a rule
-- that keeps applying is said once; ref names what it is about (the newest
-- entry time of the decisions involved, an account, a level). lesson: the
-- trader's own open lesson the nudge quotes. delivered_at: a page took it.
ALTER TABLE users ADD COLUMN guardrail INTEGER NOT NULL DEFAULT 0;

CREATE TABLE nudges (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day          TEXT NOT NULL,
    key          TEXT NOT NULL,
    ref          TEXT NOT NULL DEFAULT '',
    severity     TEXT NOT NULL,
    title        TEXT NOT NULL,
    detail       TEXT NOT NULL,
    lesson       TEXT,
    href         TEXT NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    delivered_at DATETIME,
    UNIQUE (user_id, day, key, ref)
);

-- migrate:down
DROP TABLE nudges;
ALTER TABLE users DROP COLUMN guardrail;
