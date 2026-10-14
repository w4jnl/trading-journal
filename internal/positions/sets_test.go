package positions

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// A second account of the same user, in the given copy group.
func groupedAccount(t *testing.T, q *models.Queries, userID int64, name, group string) int64 {
	t.Helper()
	apex := "apex"
	acct, err := q.CreateAccount(context.Background(), models.CreateAccountParams{
		UserID: userID, Name: name, Firm: &apex, ImportSource: "motivewave_csv", AccountRef: "TEST-" + name, Currency: "USD", CommissionSource: "rate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if group != "" {
		if _, err := q.UpdateAccount(context.Background(), models.UpdateAccountParams{Name: name, Firm: &apex, Currency: "USD", Active: 1, CopyGroup: &group, ID: acct.ID, UserID: userID}); err != nil {
			t.Fatal(err)
		}
	}
	return acct.ID
}

// Open legs cluster like closed sets, and a leg that lands after the set
// was planned inherits the set's plan: the stop price, the target and the
// thesis, with the risk from its own entry and size, marked as from the
// set. A leg with a plan of its own, and a plan saved for one leg only,
// are left alone.
func TestInheritSetPlans_OpenLegs(t *testing.T) {
	b, q, _, accA := setupBuilder(t)
	ctx := context.Background()
	accB := groupedAccount(t, q, b.UserID, "follower", "")
	accC := groupedAccount(t, q, b.UserID, "ibkr", "main")
	// The leader: long 2 ES, planned for the whole decision.
	insertSynthExecutions(t, q, accA, "ESM6", "ES", []synthFill{{"buy", 2, 7250, at(0), nil}})
	opened := execByBroker(t, q, accA, "ESM6", 0)
	stop, target := 7240.0, 7270.0
	if _, err := q.CreatePositionPlan(ctx, models.CreatePositionPlanParams{AccountID: accA, Symbol: "ESM6", OpenExecID: opened.ID, OpenedAt: opened.ExecTime,
		Stop: &stop, Risk: fptr(1000), Target: &target, Thesis: "breakout", Source: "trader", SetPlan: 1}); err != nil {
		t.Fatal(err)
	}
	// The follower: 1 MES one second later (same underlying), no plan yet.
	insertSynthExecutions(t, q, accB, "MESM6", "MES", []synthFill{{"buy", 1, 7250.25, at(0).Add(time.Second), nil}})
	// An unrelated short on the follower: a different decision.
	insertSynthExecutions(t, q, accB, "NQM6", "NQ", []synthFill{{"sell", 1, 20000, at(0), nil}})

	legs, err := OpenLegs(ctx, q, b.UserID)
	if err != nil || len(legs) != 3 {
		t.Fatalf("legs = %d (%v)", len(legs), err)
	}
	sets := ClusterOpen(legs)
	if len(sets) != 1 || len(sets[0]) != 2 {
		t.Fatalf("open sets = %v", sets)
	}
	if _, err := b.Rebuild(ctx, []Key{{accB, "MESM6"}}); err != nil {
		t.Fatal(err)
	}
	mes := execByBroker(t, q, accB, "MESM6", 0)
	got, err := q.GetPositionPlanByExec(ctx, models.GetPositionPlanByExecParams{AccountID: accB, OpenExecID: mes.ID})
	if err != nil {
		t.Fatalf("follower plan: %v", err)
	}
	if got.Source != "set" || got.SetPlan != 1 || got.Stop == nil || *got.Stop != stop || got.Target == nil || *got.Target != target || got.Thesis != "breakout" {
		t.Errorf("inherited = %+v", got)
	}
	// |7250.25 - 7240| * 5 * 1 = 51.25 (MES point value 5).
	if got.Risk == nil || !approx(*got.Risk, 51.25) {
		t.Errorf("inherited risk = %v, want 51.25", got.Risk)
	}
	nq := execByBroker(t, q, accB, "NQM6", 0)
	if _, err := q.GetPositionPlanByExec(ctx, models.GetPositionPlanByExecParams{AccountID: accB, OpenExecID: nq.ID}); err == nil {
		t.Errorf("the unrelated short must not inherit")
	}
	// Idempotent: nothing more on a second pass.
	if _, err := b.Rebuild(ctx, []Key{{accB, "MESM6"}}); err != nil {
		t.Fatal(err)
	}
	if plans, _ := q.ListOpenPositionPlansForUser(ctx, b.UserID); len(plans) != 2 {
		t.Errorf("plans after second pass = %d, want 2", len(plans))
	}

	// The IBKR leg lands 12 s late. Its account is in copy group "main";
	// the leader is not, so the two-second rule applies: not this set. Put
	// the leader in the group and it joins and inherits.
	insertSynthExecutions(t, q, accC, "MESM6", "MES", []synthFill{{"buy", 2, 7251, at(0).Add(12 * time.Second), nil}})
	if _, err := b.Rebuild(ctx, []Key{{accC, "MESM6"}}); err != nil {
		t.Fatal(err)
	}
	late := execByBroker(t, q, accC, "MESM6", 0)
	if _, err := q.GetPositionPlanByExec(ctx, models.GetPositionPlanByExecParams{AccountID: accC, OpenExecID: late.ID}); err == nil {
		t.Fatalf("a 12 s late leg outside the leader's copy group must not join")
	}
	apex, group := "apex", "main"
	if _, err := q.UpdateAccount(ctx, models.UpdateAccountParams{Name: "test", Firm: &apex, Currency: "USD", Active: 1, CopyGroup: &group, ID: accA, UserID: b.UserID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Rebuild(ctx, []Key{{accC, "MESM6"}}); err != nil {
		t.Fatal(err)
	}
	got, err = q.GetPositionPlanByExec(ctx, models.GetPositionPlanByExecParams{AccountID: accC, OpenExecID: late.ID})
	if err != nil || got.Source != "set" || got.Stop == nil || *got.Stop != stop {
		t.Fatalf("late grouped leg plan = %+v (%v)", got, err)
	}
	// |7251 - 7240| * 5 * 2 = 110.
	if got.Risk == nil || !approx(*got.Risk, 110) {
		t.Errorf("late leg risk = %v, want 110", got.Risk)
	}
}

// A closed set: the leader's leg carried a set plan into its trade; a
// follower whose trade arrives later (TopstepX lands flat) gets the plan
// on the trade and a handed-off plan row that says where it came from.
func TestInheritSetPlans_ClosedLegs(t *testing.T) {
	b, q, _, accA := setupBuilder(t)
	ctx := context.Background()
	accB := groupedAccount(t, q, b.UserID, "late", "")
	insertSynthExecutions(t, q, accA, "ESM6", "ES", []synthFill{{"buy", 1, 7250, at(0), nil}})
	opened := execByBroker(t, q, accA, "ESM6", 0)
	stop := 7240.0
	if _, err := q.CreatePositionPlan(ctx, models.CreatePositionPlanParams{AccountID: accA, Symbol: "ESM6", OpenExecID: opened.ID, OpenedAt: opened.ExecTime,
		Stop: &stop, Thesis: "t", Source: "trader", SetPlan: 1}); err != nil {
		t.Fatal(err)
	}
	insertSynthExecutions(t, q, accA, "ESM6", "ES", []synthFill{{"sell", 1, 7260, at(10), nil}})
	if _, err := b.Rebuild(ctx, []Key{{accA, "ESM6"}}); err != nil {
		t.Fatal(err)
	}
	// The follower's round trip arrives afterwards, one second behind.
	insertSynthExecutions(t, q, accB, "ESM6", "ES", []synthFill{{"buy", 2, 7250.5, at(0).Add(time.Second), nil}, {"sell", 2, 7261, at(10).Add(time.Second), nil}})
	if _, err := b.Rebuild(ctx, []Key{{accB, "ESM6"}}); err != nil {
		t.Fatal(err)
	}
	trades, _ := q.ListTradesForAccount(ctx, accB)
	if len(trades) != 1 {
		t.Fatalf("follower trades = %d", len(trades))
	}
	tr := trades[0]
	// |7250.5 - 7240| * 50 * 2 = 1050.
	if tr.PlannedStop == nil || *tr.PlannedStop != stop || tr.PlannedRisk == nil || !approx(*tr.PlannedRisk, 1050) {
		t.Fatalf("follower trade plan = %v / %v, want 7240 / 1050", tr.PlannedStop, tr.PlannedRisk)
	}
	p, err := q.GetPositionPlanByTrade(ctx, &tr.ID)
	if err != nil || p.Source != "set" || p.HandedOffAt == nil || p.SetPlan != 1 || p.Thesis != "t" {
		t.Fatalf("inherited plan row = %+v (%v)", p, err)
	}
	// A rebuild of the follower keeps the plan (the trade's own restore)
	// and re-links the row; nothing is duplicated.
	if _, err := b.Rebuild(ctx, []Key{{accB, "ESM6"}}); err != nil {
		t.Fatal(err)
	}
	trades, _ = q.ListTradesForAccount(ctx, accB)
	if trades[0].PlannedStop == nil || *trades[0].PlannedStop != stop {
		t.Errorf("plan lost on rebuild")
	}
	rows, _ := q.ListTradeSetMembersWithPlans(ctx, b.UserID)
	withPlan := 0
	for _, r := range rows {
		if r.PlanID != nil {
			withPlan++
		}
	}
	if len(rows) != 2 || withPlan != 2 {
		t.Errorf("set members = %d, with plan rows = %d, want 2 / 2", len(rows), withPlan)
	}
}
