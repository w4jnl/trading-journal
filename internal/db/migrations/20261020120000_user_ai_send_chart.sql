-- migrate:up
-- Settings > AI: send the trade's rendered chart to the model with each trade
-- or group review, for vision models (Claude, GPT-4o and later, Gemini,
-- Qwen-VL, ...). Off by default: a text-only model would refuse the image (the
-- review then retries without it, at the cost of a second call).
ALTER TABLE users ADD COLUMN ai_send_chart INTEGER NOT NULL DEFAULT 0;

-- migrate:down
ALTER TABLE users DROP COLUMN ai_send_chart;
