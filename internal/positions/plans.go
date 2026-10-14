package positions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// planSignalWindow is how far the opening fill's time may drift between the
// anchor stored on the plan and the fill now opening the cycle, when neither
// the execution id nor the order id matches: CSV and e-mail copies of one
// fill carry the same second, API sources round differently.
const planSignalWindow = 2 * time.Second

// FindPositionPlan returns the plan anchored on the fill that opened a
// position cycle, trying the anchors in order of trust: the execution id,
// then the broker order id (the MotiveWave e-mail fill is replaced by the
// CSV row under a new id, keeping its order id), then the account, contract
// and opening time within planSignalWindow. A match through a fallback
// re-points the plan to the current fill so the direct lookup works next
// time. sql.ErrNoRows when the cycle has no plan.
func FindPositionPlan(ctx context.Context, q *models.Queries, opened models.Execution) (models.PositionPlan, error) {
	plan, err := q.GetPositionPlanByExec(ctx, models.GetPositionPlanByExecParams{AccountID: opened.AccountID, OpenExecID: opened.ID})
	if err == nil {
		return plan, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return plan, fmt.Errorf("plan by exec: %w", err)
	}
	if opened.OrderID != nil && *opened.OrderID != "" {
		plan, err = q.GetPositionPlanByOrder(ctx, models.GetPositionPlanByOrderParams{AccountID: opened.AccountID, OpenOrderID: opened.OrderID})
		switch {
		case err == nil:
			return repointPlan(ctx, q, plan, opened)
		case !errors.Is(err, sql.ErrNoRows):
			return plan, fmt.Errorf("plan by order: %w", err)
		}
	}
	plan, err = q.GetPositionPlanBySignal(ctx, models.GetPositionPlanBySignalParams{
		AccountID: opened.AccountID, Symbol: opened.Symbol,
		OpenedAt: opened.ExecTime.Add(-planSignalWindow), OpenedAt_2: opened.ExecTime.Add(planSignalWindow),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return plan, err
		}
		return plan, fmt.Errorf("plan by signal: %w", err)
	}
	return repointPlan(ctx, q, plan, opened)
}

// repointPlan moves a plan found through a fallback anchor onto the fill
// that opens the cycle now, unless that fill still exists under the old id
// (two live fills in the window: the plan stays with its own).
func repointPlan(ctx context.Context, q *models.Queries, plan models.PositionPlan, opened models.Execution) (models.PositionPlan, error) {
	if plan.OpenExecID == opened.ID {
		return plan, nil
	}
	if old, err := q.GetExecutionByID(ctx, plan.OpenExecID); err == nil && old.DeletedAt == nil {
		return plan, sql.ErrNoRows
	}
	if err := q.RepointPositionPlanExec(ctx, models.RepointPositionPlanExecParams{OpenExecID: opened.ID, OpenedAt: opened.ExecTime, ID: plan.ID}); err != nil {
		return plan, fmt.Errorf("repoint plan: %w", err)
	}
	plan.OpenExecID, plan.OpenedAt = opened.ID, opened.ExecTime
	return plan, nil
}

// HandOffPlan gives a closed trade the plan of the cycle it came from: the
// first close copies stop, risk (recomputed against the trade's own entry
// and size when the stop is known) and target onto the trade and marks the
// plan handed off; every rebuild after that only re-links trade_id, since
// the trade's own plan is the one the trader edits from then on. A cycle
// without a plan is a no-op.
func HandOffPlan(ctx context.Context, q *models.Queries, opened models.Execution, trade models.Trade, inst models.Instrument) error {
	plan, err := FindPositionPlan(ctx, q, opened)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if plan.HandedOffAt == nil {
		risk := plan.Risk
		if plan.Stop != nil {
			if r := PlannedRisk(trade.EntryPrice, *plan.Stop, trade.Qty, inst); r != nil {
				risk = r
			}
		}
		if err := q.RestoreTradePlan(ctx, models.RestoreTradePlanParams{PlannedStop: plan.Stop, PlannedRisk: risk, PlannedTarget: plan.Target, ID: trade.ID}); err != nil {
			return fmt.Errorf("hand off plan %d: %w", plan.ID, err)
		}
	}
	if err := q.HandOffPositionPlan(ctx, models.HandOffPositionPlanParams{TradeID: &trade.ID, ID: plan.ID}); err != nil {
		return fmt.Errorf("link plan %d: %w", plan.ID, err)
	}
	return nil
}
