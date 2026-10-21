-- migrate:up
-- Which version of the AI method (internal/aimethod) produced the output,
-- so drafts and suggestions made under different rubrics stay comparable.
ALTER TABLE ai_outputs ADD COLUMN method TEXT NOT NULL DEFAULT '';

-- migrate:down
ALTER TABLE ai_outputs DROP COLUMN method;
