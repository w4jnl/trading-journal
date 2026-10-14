package positions

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// The opening fill of the open cycle is the plan's anchor: the first fill
// after flat, or the flip fill whose remainder opened the other way.
func TestOpenFromFills_Opened(t *testing.T) {
	stamp := func(id int64, side string, qty, price float64) models.Execution {
		return models.Execution{ID: id, Side: side, Qty: qty, Price: price, ExecTime: time.Unix(id*60, 0)}
	}
	cases := []struct {
		name   string
		fills  []models.Execution
		opened int64
		count  int
	}{
		{"single", []models.Execution{stamp(1, "buy", 2, 100)}, 1, 1},
		{"scale-in keeps the first", []models.Execution{stamp(1, "buy", 2, 100), stamp(2, "buy", 1, 101)}, 1, 2},
		{"partial exit counts", []models.Execution{stamp(1, "buy", 2, 100), stamp(2, "sell", 1, 101)}, 1, 2},
		{"flip anchors on the flip fill", []models.Execution{stamp(1, "buy", 2, 100), stamp(2, "sell", 5, 99)}, 2, 1},
		{"reopen after flat", []models.Execution{stamp(1, "buy", 1, 100), stamp(2, "sell", 1, 101), stamp(3, "buy", 2, 102)}, 3, 1},
	}
	for _, c := range cases {
		got := OpenFromFills(c.fills)
		if got.Opened.ID != c.opened || got.Fills != c.count {
			t.Errorf("%s: opened=%d fills=%d, want %d/%d", c.name, got.Opened.ID, got.Fills, c.opened, c.count)
		}
	}
	if got := OpenFromFills([]models.Execution{stamp(1, "buy", 1, 100), stamp(2, "sell", 1, 101)}); got.Opened.ID != 0 || got.Fills != 0 {
		t.Errorf("flat: opened=%d fills=%d, want zero values", got.Opened.ID, got.Fills)
	}
}

func execByBroker(t *testing.T, q *models.Queries, accID int64, symbol string, i int) models.Execution {
	t.Helper()
	rows, err := q.ListExecutionsForAccountSymbol(context.Background(), models.ListExecutionsForAccountSymbolParams{AccountID: accID, Symbol: symbol})
	if err != nil || i >= len(rows) {
		t.Fatalf("fills: %d (%v), want > %d", len(rows), err, i)
	}
	return rows[i]
}

// A plan set on the open position follows the cycle into the trade when it
// closes: stop and target copied, risk recomputed against the trade's own
// entry and size, the plan linked and marked handed off. A later rebuild
// re-links the new trade id without touching the trade's (possibly edited)
// plan.
func TestRebuild_HandsOffPositionPlan(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	ctx := context.Background()
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"buy", 1, 7252, at(2), nil}, // scale-in: avg 7251
	})
	opened := execByBroker(t, q, accID, "ESM6", 0)
	stop, target := 7240.0, 7270.0
	plan, err := q.CreatePositionPlan(ctx, models.CreatePositionPlanParams{
		AccountID: accID, Symbol: "ESM6", OpenExecID: opened.ID, OpenedAt: opened.ExecTime,
		Stop: &stop, Risk: fptr(500), Target: &target, Thesis: "test", Source: "trader",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Still open: a rebuild emits no trade and leaves the plan alone.
	if _, err := b.Rebuild(ctx, []Key{{accID, "ESM6"}}); err != nil {
		t.Fatal(err)
	}
	if p, _ := q.GetPositionPlanByID(ctx, models.GetPositionPlanByIDParams{ID: plan.ID, UserID: b.UserID}); p.HandedOffAt != nil || p.TradeID != nil {
		t.Fatalf("open cycle: plan handed off (%v / %v)", p.HandedOffAt, p.TradeID)
	}

	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{{"sell", 2, 7260, at(10), nil}})
	if _, err := b.Rebuild(ctx, []Key{{accID, "ESM6"}}); err != nil {
		t.Fatal(err)
	}
	trades, _ := q.ListTradesForAccount(ctx, accID)
	if len(trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(trades))
	}
	tr := trades[0]
	if tr.PlannedStop == nil || *tr.PlannedStop != stop || tr.PlannedTarget == nil || *tr.PlannedTarget != target {
		t.Fatalf("trade plan = %v / %v, want %g / %g", tr.PlannedStop, tr.PlannedTarget, stop, target)
	}
	// |7251 - 7240| * 50 * 2 = 1100, not the 500 typed while one lot was open.
	if tr.PlannedRisk == nil || !approx(*tr.PlannedRisk, 1100) {
		t.Fatalf("trade risk = %v, want 1100", tr.PlannedRisk)
	}
	p, err := q.GetPositionPlanByID(ctx, models.GetPositionPlanByIDParams{ID: plan.ID, UserID: b.UserID})
	if err != nil || p.HandedOffAt == nil || p.TradeID == nil || *p.TradeID != tr.ID {
		t.Fatalf("plan after close = %+v (%v), want handed off to trade %d", p, err, tr.ID)
	}

	// The trader edits the closed trade's plan; a rebuild keeps that edit and
	// re-links the plan to the new trade id.
	newStop := 7245.0
	if _, err := q.SetTradePlan(ctx, models.SetTradePlanParams{PlannedStop: &newStop, PlannedRisk: fptr(600), ID: tr.ID, UserID: b.UserID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Rebuild(ctx, []Key{{accID, "ESM6"}}); err != nil {
		t.Fatal(err)
	}
	trades, _ = q.ListTradesForAccount(ctx, accID)
	if trades[0].PlannedStop == nil || *trades[0].PlannedStop != newStop {
		t.Fatalf("rebuilt trade stop = %v, want the edited %g", trades[0].PlannedStop, newStop)
	}
	p, _ = q.GetPositionPlanByID(ctx, models.GetPositionPlanByIDParams{ID: plan.ID, UserID: b.UserID})
	if p.TradeID == nil || *p.TradeID != trades[0].ID {
		t.Fatalf("plan trade_id = %v, want rebuilt %d", p.TradeID, trades[0].ID)
	}
}

// The e-mail path: the provisional opening fill is deleted and the CSV row
// inserted under a new id with the same order id. The plan is found through
// the order id and re-pointed; with no order id, through the opening time.
func TestFindPositionPlan_Fallbacks(t *testing.T) {
	_, q, conn, accID := setupBuilder(t)
	ctx := context.Background()
	oid := "ORD-1"
	when := at(0)
	if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: accID, BrokerExecID: "MWEMAIL-ORD-1", OrderID: &oid, ImportMethod: "mw_email",
		Symbol: "ESM6", SymbolRoot: "ES", Side: "buy", Qty: 1, Price: 7250, ExecTime: when}); err != nil {
		t.Fatal(err)
	}
	mail := execByBroker(t, q, accID, "ESM6", 0)
	stop := 7240.0
	plan, err := q.CreatePositionPlan(ctx, models.CreatePositionPlanParams{AccountID: accID, Symbol: "ESM6", OpenExecID: mail.ID, OpenOrderID: &oid, OpenedAt: when, Stop: &stop, Source: "trader"})
	if err != nil {
		t.Fatal(err)
	}
	// Supersede: the e-mail row goes, the CSV row arrives.
	if _, err := conn.Exec(`DELETE FROM executions WHERE id = ?`, mail.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: accID, BrokerExecID: "CSV-1", OrderID: &oid, ImportMethod: "motivewave_csv",
		Symbol: "ESM6", SymbolRoot: "ES", Side: "buy", Qty: 1, Price: 7250, ExecTime: when.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	csv := execByBroker(t, q, accID, "ESM6", 0)
	got, err := FindPositionPlan(ctx, q, csv)
	if err != nil || got.ID != plan.ID || got.OpenExecID != csv.ID {
		t.Fatalf("by order: %+v (%v), want plan %d re-pointed to %d", got, err, plan.ID, csv.ID)
	}
	if again, err := q.GetPositionPlanByExec(ctx, models.GetPositionPlanByExecParams{AccountID: accID, OpenExecID: csv.ID}); err != nil || again.ID != plan.ID {
		t.Fatalf("direct lookup after repoint: %v", err)
	}

	// No order id anywhere: the opening time within the window matches.
	if _, err := conn.Exec(`DELETE FROM executions WHERE id = ?`, csv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: accID, BrokerExecID: "API-1", ImportMethod: "ibkr_api",
		Symbol: "ESM6", SymbolRoot: "ES", Side: "buy", Qty: 1, Price: 7250, ExecTime: when.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	api := execByBroker(t, q, accID, "ESM6", 0)
	if got, err := FindPositionPlan(ctx, q, api); err != nil || got.ID != plan.ID {
		t.Fatalf("by signal: %+v (%v)", got, err)
	}
	// A fill well outside the window is a different cycle.
	far := api
	far.ID, far.ExecTime = api.ID+1000, when.Add(time.Hour)
	if _, err := FindPositionPlan(ctx, q, far); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("far fill: err = %v, want ErrNoRows", err)
	}
}
