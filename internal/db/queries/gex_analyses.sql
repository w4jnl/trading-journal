-- name: GetGexAnalysis :one
-- The retained read for one snapshot. Keyed by the SNAPSHOT's as_of rather
-- than the requested moment, so every at-or-before query landing on the same
-- snapshot reuses the same text. as_of is RFC3339Nano UTC (see the migration).
SELECT * FROM gex_analyses
WHERE user_id = ? AND market = ? AND as_of = ?;

-- name: UpsertGexAnalysis :exec
-- Store or replace the read for a snapshot. Re-analyzing the same moment
-- overwrites in place rather than accumulating rows.
INSERT INTO gex_analyses (user_id, market, as_of, model, body)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(user_id, market, as_of) DO UPDATE SET
    body = excluded.body,
    model = excluded.model,
    created_at = CURRENT_TIMESTAMP;
