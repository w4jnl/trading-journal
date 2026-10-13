-- name: UpsertAIOutput :exec
INSERT INTO ai_outputs (user_id, kind, ref, body, data, model)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, kind, ref) DO UPDATE SET
    body = excluded.body,
    data = excluded.data,
    model = excluded.model,
    created_at = CURRENT_TIMESTAMP;

-- name: GetAIOutput :one
SELECT * FROM ai_outputs WHERE user_id = ? AND kind = ? AND ref = ?;

-- name: ListAIOutputsOfKind :many
SELECT * FROM ai_outputs WHERE user_id = ? AND kind = ? ORDER BY created_at DESC;

-- name: DeleteAIOutput :exec
DELETE FROM ai_outputs WHERE user_id = ? AND kind = ? AND ref = ?;
