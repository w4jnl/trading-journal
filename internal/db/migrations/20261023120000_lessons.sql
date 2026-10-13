-- migrate:up
-- The lessons ledger: the one lesson each in-app AI review names (a machine
-- line the review ends with, stripped from the note), kept so lessons that
-- recur can be counted, shown on the day plan and the digest, and handed to
-- the next review to check whether they were applied. mistake_key ties a
-- lesson to the label vocabulary when it is about one of those mistakes
-- (NULL otherwise). A new review of the same note replaces its lesson.
-- learned_at: the trader marked it learned; it stops counting as open.
CREATE TABLE lessons (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    entry_id    INTEGER REFERENCES journal_entries(id) ON DELETE SET NULL,
    day         TEXT NOT NULL,
    scope       TEXT NOT NULL,
    mistake_key TEXT,
    text        TEXT NOT NULL,
    learned_at  DATETIME,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_lessons_user_day ON lessons(user_id, day);

-- migrate:down
DROP INDEX idx_lessons_user_day;
DROP TABLE lessons;
