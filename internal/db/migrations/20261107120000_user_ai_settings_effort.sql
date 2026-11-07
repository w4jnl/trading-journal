-- migrate:up
-- The effort level a user's own AI provider runs at (low, medium, high, xhigh,
-- max): how long the model thinks before it writes, so the speed and the cost
-- of a review. Empty = the model's default, which is what every row had so far.
ALTER TABLE user_ai_settings ADD COLUMN effort TEXT NOT NULL DEFAULT '';

-- migrate:down
ALTER TABLE user_ai_settings DROP COLUMN effort;
