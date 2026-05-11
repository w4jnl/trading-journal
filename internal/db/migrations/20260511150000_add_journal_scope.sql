-- migrate:up
-- A `scope` discriminator on journal_entries lets the /journal feed mix day,
-- trade, and week entries cleanly. day  = entry_date alone, trade_id NULL.
--                                   trade = entry_date + trade_id set.
--                                   week  = entry_date = Monday of the week.
ALTER TABLE journal_entries ADD COLUMN scope TEXT NOT NULL DEFAULT 'day';
UPDATE journal_entries SET scope = 'trade' WHERE trade_id IS NOT NULL;

-- migrate:down
ALTER TABLE journal_entries DROP COLUMN scope;
