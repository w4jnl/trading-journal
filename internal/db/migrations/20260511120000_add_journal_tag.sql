-- migrate:up
-- Add a quick-classify tag separate from the 1-5 mood scale: good/bad/neutral.
-- Drives the tinted icon on /trades rows and /calendar cells.
ALTER TABLE journal_entries ADD COLUMN tag TEXT;

-- migrate:down
ALTER TABLE journal_entries DROP COLUMN tag;
