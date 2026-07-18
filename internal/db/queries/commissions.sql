-- name: ListAccountSymbolKeysForRoot :many
-- Inline-commission accounts (TopstepX trades CSV) are excluded: their trades
-- are the source of truth, so a FIFO rebuild would clobber the CSV-provided
-- commissions. Scoped by user_id so an admin instrument-setup recompute
-- doesn't touch other users' accounts.
SELECT DISTINCT e.account_id, e.symbol
FROM executions e
JOIN accounts a ON e.account_id = a.id
WHERE e.symbol_root = ?
  AND a.user_id = ?
  AND a.commission_source = 'rate'
ORDER BY e.account_id, e.symbol;

-- name: ListAccountSymbolKeysForCell :many
-- Same inline-account + user_id scoping as ListAccountSymbolKeysForRoot.
SELECT DISTINCT e.account_id, e.symbol
FROM executions e
JOIN accounts a ON e.account_id = a.id
JOIN instruments i ON e.symbol_root = i.symbol_root
WHERE a.firm = ?
  AND i.asset_class = ?
  AND i.size_class = ?
  AND a.user_id = ?
  AND a.commission_source = 'rate'
ORDER BY e.account_id, e.symbol;

-- name: ListAssetClasses :many
SELECT * FROM asset_classes ORDER BY sort_order, code;

-- name: ListSizeClasses :many
SELECT * FROM size_classes ORDER BY sort_order, code;

-- name: ListCommissionRates :many
-- All rate versions, grouped-by-cell order (the handler splits into cells and
-- picks each cell's current + history). effective_from ASC within a cell.
SELECT * FROM commission_rates ORDER BY asset_class, firm, size_class, effective_from;

-- name: UpsertCommissionRate :exec
-- Upsert one effective-dated version. Re-saving the same (cell, effective_from)
-- overwrites its rate; a new effective_from inserts a new version.
INSERT INTO commission_rates (asset_class, firm, size_class, effective_from, per_contract_per_side)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(asset_class, firm, size_class, effective_from) DO UPDATE SET per_contract_per_side = excluded.per_contract_per_side;

-- name: DeleteCommissionRate :exec
-- Delete one version by its stable id (a DATETIME text-equality match on the
-- 1970 migration literal is fragile, so we never delete by effective_from).
DELETE FROM commission_rates WHERE id = ?;

-- name: InsertAssetClass :exec
INSERT INTO asset_classes (code, label, sort_order) VALUES (?, ?, ?);

-- name: InsertSizeClass :exec
INSERT INTO size_classes (code, label, sort_order) VALUES (?, ?, ?);

-- name: ListOrphanedCommissionTrades :many
-- Scoped by user_id - the "you have trades whose commission cell isn't
-- configured" hint should only surface the requesting user's gaps.
SELECT
    a.id   AS account_id,
    a.name AS account_name,
    a.firm AS account_firm,
    i.symbol_root,
    i.asset_class,
    i.size_class,
    COUNT(*) AS trade_count
FROM trades t
JOIN accounts a    ON t.account_id = a.id
JOIN instruments i ON t.symbol_root = i.symbol_root
WHERE t.commission IS NULL
  AND t.status      = 'closed'
  AND t.gross_pnl  IS NOT NULL
  AND a.commission_source = 'rate'
  AND a.user_id     = ?
  -- "cell not configured" = zero rate versions for it (NOT EXISTS avoids the
  -- row fan-out a LEFT JOIN would produce now that a cell has many versions).
  AND NOT EXISTS (
      SELECT 1 FROM commission_rates cr
      WHERE cr.firm = a.firm AND cr.asset_class = i.asset_class AND cr.size_class = i.size_class
  )
GROUP BY a.id, i.symbol_root
ORDER BY trade_count DESC, a.name, i.symbol_root;

-- name: UpdateInstrumentSetup :exec
UPDATE instruments
SET tick_size = ?,
    tick_value = ?,
    multiplier = ?,
    asset_class = ?,
    size_class = ?,
    needs_setup = ?
WHERE symbol_root = ?;
