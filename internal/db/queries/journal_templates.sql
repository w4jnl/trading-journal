-- name: ListUserTemplates :many
-- The user's own (editable) templates.
SELECT * FROM journal_templates
WHERE user_id = ? AND builtin = 0
ORDER BY name;

-- name: ListBuiltinTemplates :many
-- Global system templates (read-only in the UI).
SELECT * FROM journal_templates
WHERE builtin = 1
ORDER BY name;

-- name: GetTemplate :one
-- Ownership / builtin checks happen in Go after fetch.
SELECT * FROM journal_templates WHERE id = ?;

-- name: CreateTemplate :one
INSERT INTO journal_templates (user_id, name, scope, body, builtin, updated_at)
VALUES (?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
RETURNING *;

-- name: UpdateUserTemplate :exec
-- builtin = 0 guard so a guessed id can't mutate a system template.
UPDATE journal_templates
SET name = ?, scope = ?, body = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ? AND user_id = ? AND builtin = 0;

-- name: DeleteUserTemplate :exec
DELETE FROM journal_templates
WHERE id = ? AND user_id = ? AND builtin = 0;
