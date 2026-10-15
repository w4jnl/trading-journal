-- migrate:up
-- users.auto_plan: the trader asked for every new open position to get its
-- stop and target set automatically from the AI suggestion (Settings > AI).
-- Off by default: the suggestion is then a button on the position sheet.
ALTER TABLE users ADD COLUMN auto_plan INTEGER NOT NULL DEFAULT 0;

-- migrate:down
ALTER TABLE users DROP COLUMN auto_plan;
