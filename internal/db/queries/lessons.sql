-- name: DeleteLessonsForEntry :exec
DELETE FROM lessons WHERE user_id = ? AND entry_id = ?;

-- name: InsertLesson :exec
INSERT INTO lessons (user_id, entry_id, day, scope, mistake_key, text)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListLessonsBetween :many
-- The lessons of reviews of notes dated in [from, to] (YYYY-MM-DD), newest first.
SELECT * FROM lessons
WHERE user_id = ? AND day >= ? AND day <= ?
ORDER BY day DESC, id DESC;

-- name: ListRecentLessons :many
-- The newest lessons, open or learned, other than those of one note.
SELECT * FROM lessons
WHERE user_id = ? AND (entry_id IS NULL OR entry_id != ?)
ORDER BY day DESC, id DESC
LIMIT ?;

-- name: ListOpenLessons :many
-- Lessons not marked learned, from day on, newest first.
SELECT * FROM lessons
WHERE user_id = ? AND learned_at IS NULL AND day >= ?
ORDER BY day DESC, id DESC;

-- name: MarkLessonLearned :execrows
UPDATE lessons SET learned_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ? AND learned_at IS NULL;

-- name: MarkLessonKeyLearned :execrows
-- Every open lesson about one mistake.
UPDATE lessons SET learned_at = CURRENT_TIMESTAMP WHERE user_id = ? AND mistake_key = ? AND learned_at IS NULL;
