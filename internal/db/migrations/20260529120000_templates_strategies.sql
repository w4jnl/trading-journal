-- migrate:up

-- Trading strategies: named markdown docs owned by a user. A journal entry
-- may reference at most one (journal_entries.strategy_id below). Later an AI
-- step will analyze trades against the assigned strategy; that link is the
-- only hook needed now.
CREATE TABLE strategies (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, name)
);
CREATE INDEX idx_strategies_user ON strategies(user_id);

-- One strategy (or none) per entry. ON DELETE SET NULL so removing a
-- strategy detaches it from entries rather than deleting the entries.
ALTER TABLE journal_entries
    ADD COLUMN strategy_id INTEGER REFERENCES strategies(id) ON DELETE SET NULL;

-- updated_at on templates so the Library can track edits (table had only
-- created_at). Nullable; the edit path sets it.
ALTER TABLE journal_templates
    ADD COLUMN updated_at DATETIME;

-- migrate:down
ALTER TABLE journal_templates DROP COLUMN updated_at;
ALTER TABLE journal_entries DROP COLUMN strategy_id;
DROP INDEX idx_strategies_user;
DROP TABLE strategies;
