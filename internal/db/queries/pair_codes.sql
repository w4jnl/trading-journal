-- Pairing codes: a phone exchanges one for a device key (see api_pair.go).

-- name: CreatePairCode :one
INSERT INTO pair_codes (user_id, code_sha256, device_hint, expires_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetPairCodeBySHA256 :one
-- The code with its user, whatever its state; the handler checks expiry
-- and use so a reused or stale code fails the same way as a wrong one.
SELECT p.id, p.user_id, p.device_hint, p.created_at, p.expires_at, p.used_at, u.is_active
FROM pair_codes p
JOIN users u ON u.id = p.user_id
WHERE p.code_sha256 = ?;

-- name: UsePairCode :execrows
-- Marks the code used; zero rows means another request won the race.
UPDATE pair_codes SET used_at = CURRENT_TIMESTAMP WHERE id = ? AND used_at IS NULL;

-- name: ExpirePairCodesForUser :exec
-- A new code retires the unused ones before it: one live code per user.
UPDATE pair_codes SET expires_at = CURRENT_TIMESTAMP WHERE user_id = ? AND used_at IS NULL AND expires_at > CURRENT_TIMESTAMP;

-- name: DeleteOldPairCodes :exec
DELETE FROM pair_codes WHERE expires_at < ?;
