-- migrate:up

-- journal_entries needs its own user_id column because day/week/group
-- entries have account_id = NULL and can't reach the user via FK chain.
-- Trade-scope entries set account_id, but the user_id column is the
-- canonical scope - the importer / handlers always set it from the
-- request context.
PRAGMA defer_foreign_keys = ON;

ALTER TABLE journal_entries
    ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE CASCADE;

-- Backfill from the linked account where present; remaining rows
-- (day/week/group) inherit the bootstrap user.
UPDATE journal_entries
SET user_id = (SELECT a.user_id FROM accounts a WHERE a.id = account_id)
WHERE account_id IS NOT NULL;

UPDATE journal_entries
SET user_id = (SELECT id FROM users WHERE username = '${BOOTSTRAP_USERNAME}')
WHERE user_id IS NULL;

CREATE INDEX idx_journal_entries_user_scope_date
    ON journal_entries(user_id, scope, entry_date DESC);

-- migrate:down
DROP INDEX idx_journal_entries_user_scope_date;
ALTER TABLE journal_entries DROP COLUMN user_id;
