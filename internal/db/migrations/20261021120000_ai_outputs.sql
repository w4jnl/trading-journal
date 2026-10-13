-- migrate:up
-- What the in-app AI produced that is not a note: a strategy draft, the
-- strategies found in untagged trades, a trade's proposed levels, the
-- stop/target lab's reading, a day plan draft. One row per (user, kind,
-- ref) - ref says what it is about (a strategy id, a trade id, a date, 'all')
-- and a newer run replaces it. body is markdown for the trader; data is JSON
-- the app acts on (the checklist lines, the clusters, the levels); model
-- names who wrote it. Nothing here changes the journal until the trader
-- applies it.
CREATE TABLE ai_outputs (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    ref        TEXT NOT NULL,
    body       TEXT NOT NULL,
    data       TEXT NOT NULL DEFAULT '{}',
    model      TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, kind, ref)
);

-- migrate:down
DROP TABLE ai_outputs;
