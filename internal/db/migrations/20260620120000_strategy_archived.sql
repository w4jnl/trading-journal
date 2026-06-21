-- migrate:up
-- Soft-retire a strategy without deleting it. An archived strategy drops to the
-- bottom of the Library, can no longer be newly assigned, and is hidden from the
-- AI analyzers. Entries that already reference it keep the link (archived_at is
-- only a flag) and surface it as "retired".
ALTER TABLE strategies ADD COLUMN archived_at DATETIME;

-- migrate:down
ALTER TABLE strategies DROP COLUMN archived_at;
