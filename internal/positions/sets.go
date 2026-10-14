package positions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- Copy trades: open sets and inherited plans ------------------------
//
// A copier fans one decision across accounts; the legs reach the journal at
// different speeds (an e-mail fill in seconds, an IBKR fill on the next
// poll, a TopstepX leg only once closed). The trader plans the decision on
// whichever leg is visible; the others inherit it when they land. Nothing
// is stored for an open set: the legs cluster on their opening fills every
// time (copyset.Cluster, same rule as closed sets), and the plan is per
// leg, marked set_plan when it was saved for the whole decision.

// OpenLeg is one open position cycle with what clustering and inheriting
// need.
type OpenLeg struct {
	AccountID  int64
	Symbol     string
	SymbolRoot string
	CopyGroup  string
	Position   OpenPosition
	Plan       *models.PositionPlan // nil when the cycle has no plan
}

// OpenLegs replays the user's open positions into legs: the fills of each
// (account, contract) that does not net flat, the cycle's opening fill and
// its plan.
func OpenLegs(ctx context.Context, q *models.Queries, userID int64) ([]OpenLeg, error) {
	rows, err := q.ListOpenPositions(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list open positions: %w", err)
	}
	groups := map[int64]string{}
	legs := make([]OpenLeg, 0, len(rows))
	for _, r := range rows {
		fills, err := q.ListExecutionsForAccountSymbol(ctx, models.ListExecutionsForAccountSymbolParams{AccountID: r.AccountID, Symbol: r.Symbol})
		if err != nil {
			return nil, fmt.Errorf("fills %d/%s: %w", r.AccountID, r.Symbol, err)
		}
		pos := OpenFromFills(fills)
		if pos.NetQty == 0 {
			continue
		}
		leg := OpenLeg{AccountID: r.AccountID, Symbol: r.Symbol, SymbolRoot: r.SymbolRoot, Position: pos}
		if g, ok := groups[r.AccountID]; ok {
			leg.CopyGroup = g
		} else if acct, err := q.GetAccountByID(ctx, models.GetAccountByIDParams{ID: r.AccountID, UserID: userID}); err == nil && acct.CopyGroup != nil {
			leg.CopyGroup = *acct.CopyGroup
			groups[r.AccountID] = leg.CopyGroup
		}
		plan, err := FindPositionPlan(ctx, q, pos.Opened)
		switch {
		case err == nil && plan.HandedOffAt == nil:
			leg.Plan = &plan
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		legs = append(legs, leg)
	}
	return legs, nil
}

// Direction of an open leg.
func (l OpenLeg) Direction() string {
	if l.Position.NetQty < 0 {
		return "short"
	}
	return "long"
}

// Qty is the leg's size, unsigned.
func (l OpenLeg) Qty() float64 {
	if l.Position.NetQty < 0 {
		return -l.Position.NetQty
	}
	return l.Position.NetQty
}

// ClusterOpen groups open legs into sets (two or more accounts, same
// underlying and direction, opening fills within the window): index groups
// into legs, like copyset.Cluster.
func ClusterOpen(legs []OpenLeg) [][]int {
	trs := make([]copyset.Trade, len(legs))
	for i, l := range legs {
		trs[i] = copyset.Trade{ID: int64(i), AccountID: l.AccountID, SymbolRoot: l.SymbolRoot, Direction: l.Direction(), EntryTime: l.Position.Opened.ExecTime, CopyGroup: l.CopyGroup}
	}
	sets, _ := copyset.Cluster(trs)
	return sets
}

// InheritSetPlans gives legs that joined a set without a plan of their own
// the set's plan: among the open sets, and among the closed sets whose
// members were rebuilt. The stop is the price (what a copier mirrors on
// the stop order), the target and the thesis are the same, the risk is the
// leg's own; the row carries source 'set' and stays marked as a set plan
// so a leg landing even later inherits it in turn. A leg with any plan of
// its own is left alone. Runs after every rebuild of the user's trades; it
// only ever fills empty slots, so it is idempotent.
func InheritSetPlans(ctx context.Context, q *models.Queries, userID int64, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	n, err := inheritOpen(ctx, q, userID)
	if err != nil {
		return err
	}
	m, err := inheritClosed(ctx, q, userID)
	if err != nil {
		return err
	}
	if n+m > 0 {
		log.Info("set plans inherited", "user", userID, "open_legs", n, "closed_legs", m)
	}
	return nil
}

func inheritOpen(ctx context.Context, q *models.Queries, userID int64) (int, error) {
	legs, err := OpenLegs(ctx, q, userID)
	if err != nil {
		return 0, err
	}
	n := 0
	insts := map[string]models.Instrument{}
	for _, set := range ClusterOpen(legs) {
		src := setPlanOf(legs, set)
		if src == nil {
			continue
		}
		for _, k := range set {
			leg := legs[k]
			if leg.Plan != nil {
				continue
			}
			risk := legRisk(ctx, q, insts, leg.SymbolRoot, leg.Position.AvgPrice, src.Stop, leg.Qty())
			if _, err := q.CreatePositionPlan(ctx, models.CreatePositionPlanParams{
				AccountID: leg.AccountID, Symbol: leg.Symbol, OpenExecID: leg.Position.Opened.ID, OpenOrderID: leg.Position.Opened.OrderID, OpenedAt: leg.Position.Opened.ExecTime,
				Stop: src.Stop, Risk: risk, Target: src.Target, Thesis: src.Thesis, Source: "set", SetPlan: 1,
			}); err != nil {
				return n, fmt.Errorf("inherit plan for %d/%s: %w", leg.AccountID, leg.Symbol, err)
			}
			n++
		}
	}
	return n, nil
}

// setPlanOf picks the plan the set's legs inherit: the first leg (by
// opening time) whose plan was saved for the whole decision.
func setPlanOf(legs []OpenLeg, set []int) *models.PositionPlan {
	for _, k := range set {
		if p := legs[k].Plan; p != nil && p.SetPlan != 0 {
			return p
		}
	}
	return nil
}

func inheritClosed(ctx context.Context, q *models.Queries, userID int64) (int, error) {
	rows, err := q.ListTradeSetMembersWithPlans(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list set members: %w", err)
	}
	n := 0
	insts := map[string]models.Instrument{}
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].SetID == rows[i].SetID {
			j++
		}
		members := rows[i:j]
		i = j
		var src *models.ListTradeSetMembersWithPlansRow
		for k := range members {
			if members[k].PlanID != nil && members[k].SetPlan != nil && *members[k].SetPlan != 0 && (members[k].PlannedStop != nil || members[k].PlannedTarget != nil) {
				src = &members[k]
				break
			}
		}
		if src == nil {
			continue
		}
		for _, m := range members {
			if m.PlannedStop != nil || m.PlannedRisk != nil || m.PlannedTarget != nil || m.PlanID != nil {
				continue
			}
			risk := legRisk(ctx, q, insts, m.SymbolRoot, m.EntryPrice, src.PlannedStop, m.Qty)
			if err := q.RestoreTradePlan(ctx, models.RestoreTradePlanParams{PlannedStop: src.PlannedStop, PlannedRisk: risk, PlannedTarget: src.PlannedTarget, ID: m.ID}); err != nil {
				return n, fmt.Errorf("inherit plan for trade %d: %w", m.ID, err)
			}
			// Record where it came from, anchored like any plan, so the
			// trade sheet can say so and a rebuild keeps the link.
			execID, err := q.FirstEntryExecutionForTrade(ctx, m.ID)
			if err != nil {
				continue
			}
			opened, err := q.GetExecutionByID(ctx, execID)
			if err != nil {
				continue
			}
			thesis := ""
			if src.Thesis != nil {
				thesis = *src.Thesis
			}
			tradeID := m.ID
			if _, err := q.CreateHandedOffPositionPlan(ctx, models.CreateHandedOffPositionPlanParams{
				AccountID: m.AccountID, Symbol: m.Symbol, OpenExecID: opened.ID, OpenOrderID: opened.OrderID, OpenedAt: opened.ExecTime,
				Stop: src.PlannedStop, Risk: risk, Target: src.PlannedTarget, Thesis: thesis, Source: "set", SetPlan: 1, TradeID: &tradeID,
			}); err != nil {
				return n, fmt.Errorf("record inherited plan for trade %d: %w", m.ID, err)
			}
			n++
		}
	}
	return n, nil
}

// legRisk is the money at risk of one leg from the set's stop at the leg's
// own entry and size, nil without a stop or tick specs.
func legRisk(ctx context.Context, q *models.Queries, insts map[string]models.Instrument, root string, entry float64, stop *float64, qty float64) *float64 {
	if stop == nil {
		return nil
	}
	inst, ok := insts[root]
	if !ok {
		inst, _ = q.GetInstrument(ctx, root)
		insts[root] = inst
	}
	return PlannedRisk(entry, *stop, qty, inst)
}
