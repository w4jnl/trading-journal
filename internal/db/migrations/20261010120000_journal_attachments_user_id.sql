-- migrate:up
-- Attachments gain an owner. The row only knew entry_id, which the save
-- handlers never fill in (bodies are the source of truth for which images an
-- entry uses), so the browser serve path fell back to "entry_id = 0 is
-- readable by anyone signed in" - and ids are sequential, so every user's
-- screenshots were readable cross-tenant. From now on the uploader is
-- stamped on the row and the serve query is WHERE user_id = caller.
--
-- Backfill: the journal entry or strategy body that references the image
-- names its owner; anything unreferenced falls to the bootstrap user. The
-- GLOB mirrors the handlers' reference check (an id is always followed by a
-- non-digit in a markdown image URL, so 1 never matches /attachments/12).
ALTER TABLE journal_attachments ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE CASCADE;

UPDATE journal_attachments SET user_id = (
    SELECT je.user_id FROM journal_entries je
    WHERE je.user_id IS NOT NULL
      AND je.body GLOB ('*/journal/attachments/' || journal_attachments.id || '[^0-9]*')
    LIMIT 1)
WHERE user_id IS NULL;

UPDATE journal_attachments SET user_id = (
    SELECT s.user_id FROM strategies s
    WHERE s.body GLOB ('*/journal/attachments/' || journal_attachments.id || '[^0-9]*')
    LIMIT 1)
WHERE user_id IS NULL;

UPDATE journal_attachments SET user_id = (SELECT id FROM users WHERE username = '${BOOTSTRAP_USERNAME}')
WHERE user_id IS NULL;

CREATE INDEX idx_journal_attachments_user ON journal_attachments(user_id);

-- migrate:down
DROP INDEX idx_journal_attachments_user;
ALTER TABLE journal_attachments DROP COLUMN user_id;
