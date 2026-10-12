package coach

import (
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/dayplan"
)

// The plan read against the day: bias, the decision cap, the stop time
// and the max loss per account, with a finding per broken rule; the norm
// checks the plan supersedes stay quiet; a plan written after the first
// trade is flagged.
func TestPlanOutcome(t *testing.T) {
	tz := time.UTC
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, tz)
	i64 := func(v int64) *int64 { return &v }
	// Decisions (14:00 + seconds): a long loss, a short win, two more longs
	// losing on account 1 (cumulative -60 by 14:20), then a long after 15:00.
	today := []Trade{
		mk(1, 1, "MES", "long", 1, 0, 60, -30, day),
		mk(2, 1, "MES", "short", 1, 600, 660, 10, day),
		mk(3, 1, "MES", "long", 1, 1000, 1100, -20, day),
		mk(4, 1, "MES", "long", 1, 1150, 1200, -20, day), // account 1 at -60 here
		mk(5, 1, "MES", "long", 1, 4000, 4030, 5, day),   // 15:06:40, after the stop and the breach
	}
	plan := &dayplan.Plan{Day: "2026-10-09", Bias: "short", MaxTrades: i64(3), StopTime: "15:00", MaxLoss: f(50),
		CreatedAt: day.Add(8 * time.Hour), UpdatedAt: day.Add(8 * time.Hour)}

	// A long history so the norm checks would speak if the plan did not supersede them.
	var history []Trade
	id := int64(1000)
	for d := 1; d <= 10; d++ {
		for k := 0; k < 2; k++ {
			history = append(history, mk(id, 1, "MES", "short", 1, k*300, k*300+40, 5, day.AddDate(0, 0, -d)))
			id++
		}
	}
	r := Analyze(day, tz, today, history, plan)
	if r.Plan == nil || len(r.Plan.Checks) != 4 || r.Plan.Held != 0 || r.Plan.Broken() != 4 || r.Plan.WrittenLate || r.Plan.EditedLate {
		t.Fatalf("plan outcome = %+v", r.Plan)
	}
	byKey := map[string]PlanCheck{}
	for _, c := range r.Plan.Checks {
		byKey[c.Key] = c
	}
	if c := byKey["bias"]; c.Rule != "Short bias" || c.Actual != "1 with it (+10.00), 4 against (-65.00)" {
		t.Errorf("bias check = %+v", c)
	}
	if c := byKey["max-trades"]; c.Rule != "At most 3 decision(s)" || c.Actual != "took 5" {
		t.Errorf("max trades check = %+v", c)
	}
	if c := byKey["stop-time"]; c.Rule != "Done by 15:00" || c.Actual != "last entry 15:06" {
		t.Errorf("stop time check = %+v", c)
	}
	if c := byKey["max-loss"]; c.Actual != "hit at 14:20 on acct1, 1 decision(s) after" {
		t.Errorf("max loss check = %+v", c)
	}
	got := strings.Join(keys(r), ",")
	for _, want := range []string{"plan-bias", "plan-max-trades", "plan-stop-time", "plan-max-loss"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if !strings.HasPrefix(got, "plan-") {
		t.Errorf("plan findings should lead: %s", got)
	}
	for _, no := range []string{"overtraded", "late-session"} {
		if strings.Contains(got, no) {
			t.Errorf("%s should be superseded by the plan: %s", no, got)
		}
	}
	for _, x := range r.Findings {
		if x.Key == "plan-max-trades" && (len(x.TradeIDs) != 2 || x.TradeIDs[0] != 4 || x.Label.Key != "overtraded" || !strings.Contains(x.Detail, "decision 4 opened at 14:19")) {
			t.Errorf("max trades finding = %+v", x)
		}
		if x.Key == "plan-max-loss" && (len(x.TradeIDs) != 1 || x.TradeIDs[0] != 5 || x.Label.Key != "against-plan") {
			t.Errorf("max loss finding = %+v", x)
		}
	}

	// Stopped at the limit, inside every rule, written late.
	held := today[:4]
	plan2 := &dayplan.Plan{Day: "2026-10-09", Bias: "neutral", MaxTrades: i64(4), StopTime: "15:00", MaxLoss: f(50),
		CreatedAt: day.Add(14*time.Hour + 5*time.Minute), UpdatedAt: day.Add(14*time.Hour + 5*time.Minute)}
	r = Analyze(day, tz, held, nil, plan2)
	if r.Plan.Held != 3 || len(r.Plan.Checks) != 3 || !r.Plan.WrittenLate {
		t.Errorf("held plan = %+v", r.Plan)
	}
	for _, c := range r.Plan.Checks {
		if c.Key == "max-loss" && c.Actual != "hit at 14:20 on acct1, stopped there" {
			t.Errorf("stopped-at-limit check = %+v", c)
		}
	}
	// Stay out: any decision breaks it. No trades: no checks, plan carried.
	r = Analyze(day, tz, today[:1], nil, &dayplan.Plan{Day: "2026-10-09", Bias: "flat"})
	if len(r.Plan.Checks) != 1 || r.Plan.Checks[0].Rule != "Stay out" || r.Plan.Held != 0 {
		t.Errorf("flat plan = %+v", r.Plan)
	}
	if r = Analyze(day, tz, nil, nil, plan); r.Plan == nil || len(r.Plan.Checks) != 0 {
		t.Errorf("no trades = %+v", r.Plan)
	}
}
