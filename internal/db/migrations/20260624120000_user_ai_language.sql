-- migrate:up
-- Per-user language for the in-app (local) AI analyzer. Empty string = the model's
-- default (English); a value like "Czech" makes the analyzer write reviews in that
-- language. Does NOT localize the web UI or the external MCP/ChatGPT analyzers.
ALTER TABLE users ADD COLUMN ai_language TEXT NOT NULL DEFAULT '';

-- migrate:down
ALTER TABLE users DROP COLUMN ai_language;
