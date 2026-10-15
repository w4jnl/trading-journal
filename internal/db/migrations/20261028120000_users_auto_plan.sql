-- migrate:up
-- users.auto_plan: what happens to a new open position (Settings > AI):
-- 0 nothing (the AI suggestion is a button on the position sheet),
-- 1 the suggestion is made and waits on the sheet,
-- 2 the suggestion is made and written into the plan (source 'ai').
ALTER TABLE users ADD COLUMN auto_plan INTEGER NOT NULL DEFAULT 0;

-- migrate:down
ALTER TABLE users DROP COLUMN auto_plan;
