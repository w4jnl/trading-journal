-- migrate:up
-- The trader's profile for the AI method (internal/aimethod.Profile) as
-- JSON: style (scalper, daytrader, swing), the evidence they weigh most,
-- their markets and free-text house rules. Empty = the defaults.
ALTER TABLE users ADD COLUMN ai_profile TEXT NOT NULL DEFAULT '';

-- migrate:down
ALTER TABLE users DROP COLUMN ai_profile;
