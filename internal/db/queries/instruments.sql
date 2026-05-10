-- name: UpsertInstrumentStub :exec
INSERT INTO instruments (symbol_root, exchange, currency, needs_setup)
VALUES (?, ?, 'USD', 1)
ON CONFLICT(symbol_root) DO NOTHING;

-- name: GetInstrument :one
SELECT * FROM instruments WHERE symbol_root = ?;

-- name: ListInstruments :many
SELECT * FROM instruments ORDER BY symbol_root;
