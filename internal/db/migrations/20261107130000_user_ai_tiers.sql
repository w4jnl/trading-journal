-- migrate:up
-- The model and effort a user's AI tasks run at, by difficulty tier (quick,
-- standard, deep). Each AI call names its task, the task has a tier, and a
-- row here overrides the user's main model and effort for that tier; a blank
-- model or effort keeps the main one. No row = every task on the main model.
CREATE TABLE user_ai_tiers (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tier       TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    effort     TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, tier)
);

-- migrate:down
DROP TABLE user_ai_tiers;
