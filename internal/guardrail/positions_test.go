package guardrail

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// A position open for a few minutes with no stop is pointed out once, with
// a link to its sheet; recording the stop ends it. Open risk past what the
// plan's max loss leaves after the day's losses is pointed out too. Legs
// of one copied decision share the nudge.
func TestOpenPositionNudges(t *testing.T) {
	s, conn, uid := guardFixture(t)
	ctx := context.Background()
	q := models.New(conn)
	// A second account for the copied leg.
	if _, err := conn.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (2, ?, 'beta', 'motivewave_csv', 'B')`, uid); err != nil {
		t.Fatal(err)
	}
	opened := at(14, 5, 30)
	for _, f := range []struct {
		acct  int64
		id    string
		qty   float64
		price float64
		when  time.Time
	}{{1, "O1", 2, 7000, opened}, {2, "O2", 1, 7000.25, opened.Add(time.Second)}} {
		if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: f.acct, BrokerExecID: f.id, ImportMethod: "motivewave_csv",
			Symbol: "ESZ6", SymbolRoot: "ES", Side: "buy", Qty: f.qty, Price: f.price, ExecTime: f.when}); err != nil {
			t.Fatal(err)
		}
	}
	// Two minutes in: too early to complain.
	now := at(14, 7, 0)
	s.Now = func() time.Time { return now }
	got, err := s.Check(ctx, uid, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range got {
		if n.Key == "unplanned-open" {
			t.Fatalf("nudged after two minutes: %+v", n)
		}
	}
	// Four minutes in: once, for the decision, linking to the first leg.
	now = at(14, 9, 45)
	got, _ = s.Check(ctx, uid, false)
	var open *models.Nudge
	for i := range got {
		if got[i].Key == "unplanned-open" {
			open = &got[i]
		}
	}
	if open == nil {
		t.Fatalf("no unplanned-open nudge: %+v", got)
	}
	if !strings.Contains(open.Detail, "long ESZ6 on 2 accounts") || !strings.Contains(open.Href, "sheet=pos%3A1%3AESZ6") {
		t.Errorf("nudge = %+v", open)
	}
	if again, _ := s.Check(ctx, uid, false); len(again) != 0 {
		t.Errorf("said twice: %+v", again)
	}

	// The trader records a stop on the first leg risking 300 on account 1,
	// where the day is already 120 down against a max loss of 100: the
	// plan leaves nothing, so the open risk is past it.
	first, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{AccountID: 1, BrokerExecID: "O1"})
	if err != nil {
		t.Fatal(err)
	}
	stop := 6997.0
	if _, err := q.CreatePositionPlan(ctx, models.CreatePositionPlanParams{AccountID: 1, Symbol: "ESZ6", OpenExecID: first.ID, OpenedAt: first.ExecTime,
		Stop: &stop, Risk: f64(300), Source: "trader"}); err != nil {
		t.Fatal(err)
	}
	now = at(14, 10, 30)
	got, _ = s.Check(ctx, uid, true)
	var risk *models.Nudge
	for i := range got {
		switch got[i].Key {
		case "open-risk":
			risk = &got[i]
		case "unplanned-open":
			t.Errorf("planned decision still nudged: %+v", got[i])
		}
	}
	if risk == nil || !strings.Contains(risk.Title, "alpha") || !strings.Contains(risk.Detail, "risk 300.00") || !strings.Contains(risk.Detail, "leaves 0.00") {
		t.Fatalf("open-risk nudge = %+v (all: %+v)", risk, got)
	}
	var n int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM nudges WHERE user_id = ? AND key IN ('unplanned-open', 'open-risk')`, uid).Scan(&n)
	if n != 2 {
		t.Errorf("%d position nudges stored, want 2", n)
	}
}

// A position opened long before the window (carried from an earlier
// session) is never nudged about.
func TestOpenPositionNudges_StaleIsQuiet(t *testing.T) {
	s, conn, uid := guardFixture(t)
	ctx := context.Background()
	q := models.New(conn)
	if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: 1, BrokerExecID: "OLD", ImportMethod: "motivewave_csv",
		Symbol: "NQZ6", SymbolRoot: "NQ", Side: "sell", Qty: 1, Price: 20000, ExecTime: at(9, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return at(14, 6, 0) }
	got, err := s.Check(ctx, uid, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range got {
		if n.Key == "unplanned-open" {
			t.Errorf("stale position nudged: %+v", n)
		}
	}
}
