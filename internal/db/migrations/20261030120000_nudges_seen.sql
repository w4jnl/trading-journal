-- migrate:up
-- nudges.seen_at: the trader acknowledged the nudge anywhere (a tap or a
-- dismiss on the phone, a page that took it). delivered_at keeps meaning
-- "a web page took it"; the phone app's badge counts today's nudges with
-- no seen_at.
ALTER TABLE nudges ADD COLUMN seen_at DATETIME;

-- migrate:down
ALTER TABLE nudges DROP COLUMN seen_at;
