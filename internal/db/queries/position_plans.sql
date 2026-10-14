-- name: GetPositionPlanByExec :one
-- The plan anchored on this opening fill (any state: open or handed off).
SELECT * FROM position_plans WHERE account_id = ? AND open_exec_id = ?;

-- name: GetPositionPlanByOrder :one
-- Fallback anchor: the opening fill was replaced under a new id (e-mail fill
-- superseded by the CSV row) but kept its broker order id. Newest first in
-- case an order id was reused.
SELECT * FROM position_plans
WHERE account_id = ? AND open_order_id = ?
ORDER BY id DESC LIMIT 1;

-- name: GetPositionPlanBySignal :one
-- Last fallback: same account, contract and opening time (within the window
-- the caller passes as lower/upper bounds).
SELECT * FROM position_plans
WHERE account_id = ? AND symbol = ? AND opened_at >= ? AND opened_at <= ?
ORDER BY id DESC LIMIT 1;

-- name: GetPositionPlanByID :one
-- One plan, scoped to the caller through the accounts table.
SELECT pp.* FROM position_plans pp
JOIN accounts a ON a.id = pp.account_id
WHERE pp.id = ? AND a.user_id = ?;

-- name: CreatePositionPlan :one
INSERT INTO position_plans (account_id, symbol, open_exec_id, open_order_id, opened_at, stop, risk, target, thesis, source, set_plan)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: CreateHandedOffPositionPlan :one
-- A plan written straight onto a closed trade (a leg that joined its set
-- after the set was planned): anchored on the trade's opening fill like
-- any other, already linked and handed off.
INSERT INTO position_plans (account_id, symbol, open_exec_id, open_order_id, opened_at, stop, risk, target, thesis, source, set_plan, trade_id, handed_off_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
RETURNING *;

-- name: UpdatePositionPlan :exec
-- The trader's edit: levels, thesis, who set them and whether it is the
-- whole decision's plan. The hand-off state is left alone.
UPDATE position_plans
SET stop = ?, risk = ?, target = ?, thesis = ?, source = ?, set_plan = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ?;

-- name: GetPositionPlanByTrade :one
-- The plan a closed trade came from, if its cycle was planned while open
-- (or inherited from its set): the trade sheet shows where the plan came from.
SELECT * FROM position_plans WHERE trade_id = ? ORDER BY id DESC LIMIT 1;

-- name: RepointPositionPlanExec :exec
-- The opening fill got a new execution id (CSV superseded the e-mail row):
-- move the anchor so the direct lookup works again.
UPDATE position_plans SET open_exec_id = ?, opened_at = ? WHERE id = ?;

-- name: SetPositionPlanRisk :exec
-- The position changed size (scale-in, partial exit): the money at risk
-- follows the stop. Only the risk moves; the stop is the trader's.
UPDATE position_plans SET risk = ? WHERE id = ?;

-- name: HandOffPositionPlan :exec
-- The cycle closed: the plan now belongs to this trade. handed_off_at is
-- set once (the first close); later rebuilds only re-point trade_id.
UPDATE position_plans
SET trade_id = ?, handed_off_at = COALESCE(handed_off_at, CURRENT_TIMESTAMP)
WHERE id = ?;

-- name: ListOpenPositionPlansForUser :many
-- Plans not yet handed off, for the open-position views: one per open cycle
-- the trader planned. Scoped to the user's accounts.
SELECT pp.* FROM position_plans pp
JOIN accounts a ON a.id = pp.account_id
WHERE a.user_id = ? AND pp.handed_off_at IS NULL
ORDER BY pp.account_id, pp.symbol;

-- name: DeletePositionPlan :exec
DELETE FROM position_plans WHERE id = ?;
