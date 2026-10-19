package guardrail

import (
	"context"
	"strconv"
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

// The newest cached price through the recorded stop is said once per leg,
// only while that price is fresh, and never without a stop.
func TestOpenPositionNudges_ThroughStop(t *testing.T) {
	s, conn, uid := guardFixture(t)
	ctx := context.Background()
	q := models.New(conn)
	opened := at(14, 5, 0)
	if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: 1, BrokerExecID: "L1", ImportMethod: "motivewave_csv",
		Symbol: "ESZ6", SymbolRoot: "ES", Side: "buy", Qty: 1, Price: 7000, ExecTime: opened}); err != nil {
		t.Fatal(err)
	}
	first, _ := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{AccountID: 1, BrokerExecID: "L1"})
	price, priceAt := 6999.0, at(14, 6, 0)
	s.LastClose = func(context.Context, string) (float64, time.Time, bool) { return price, priceAt, true }
	s.Now = func() time.Time { return at(14, 6, 30) }
	got, _ := s.Check(ctx, uid, true)
	for _, n := range got {
		if n.Key == "past-stop-live" {
			t.Fatalf("no stop recorded, yet: %+v", n)
		}
	}
	stop := 6997.0
	if _, err := q.CreatePositionPlan(ctx, models.CreatePositionPlanParams{AccountID: 1, Symbol: "ESZ6", OpenExecID: first.ID, OpenedAt: first.ExecTime, Stop: &stop, Risk: f64(150), Source: "trader"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Check(ctx, uid, true)
	for _, n := range got {
		if n.Key == "past-stop-live" {
			t.Fatalf("price above the stop, yet: %+v", n)
		}
	}
	price = 6996.5
	got, _ = s.Check(ctx, uid, false)
	var hit *models.Nudge
	for i := range got {
		if got[i].Key == "past-stop-live" {
			hit = &got[i]
		}
	}
	if hit == nil || !strings.Contains(hit.Detail, "Last 6996.50 against the recorded stop 6997") || !strings.Contains(hit.Href, "sheet=pos%3A1%3AESZ6") {
		t.Fatalf("through-stop nudge = %+v (all %+v)", hit, got)
	}
	if again, _ := s.Check(ctx, uid, false); len(again) != 0 {
		t.Errorf("said twice: %+v", again)
	}
	// A stale price (an hour old) never nudges.
	s2, conn2, uid2 := guardFixture(t)
	q2 := models.New(conn2)
	if _, err := q2.InsertExecution(ctx, models.InsertExecutionParams{AccountID: 1, BrokerExecID: "L2", ImportMethod: "motivewave_csv",
		Symbol: "ESZ6", SymbolRoot: "ES", Side: "buy", Qty: 1, Price: 7000, ExecTime: opened}); err != nil {
		t.Fatal(err)
	}
	e2, _ := q2.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{AccountID: 1, BrokerExecID: "L2"})
	if _, err := q2.CreatePositionPlan(ctx, models.CreatePositionPlanParams{AccountID: 1, Symbol: "ESZ6", OpenExecID: e2.ID, OpenedAt: e2.ExecTime, Stop: &stop, Source: "trader"}); err != nil {
		t.Fatal(err)
	}
	s2.LastClose = func(context.Context, string) (float64, time.Time, bool) { return 6990, at(13, 0, 0), true }
	s2.Now = func() time.Time { return at(14, 6, 30) }
	got, _ = s2.Check(ctx, uid2, true)
	for _, n := range got {
		if n.Key == "past-stop-live" {
			t.Errorf("stale price nudged: %+v", n)
		}
	}
}

// Two copied legs through the same stop are one nudge, on the first leg,
// naming the accounts; a leg with a stop of its own is said separately.
func TestOpenPositionNudges_ThroughStopSet(t *testing.T) {
	s, conn, uid := guardFixture(t)
	ctx := context.Background()
	q := models.New(conn)
	if _, err := conn.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (2, ?, 'beta', 'motivewave_csv', 'B'), (3, ?, 'gamma', 'motivewave_csv', 'C')`, uid, uid); err != nil {
		t.Fatal(err)
	}
	opened := at(14, 5, 0)
	stops := map[int64]float64{1: 6997, 2: 6997, 3: 6998}
	for _, acct := range []int64{1, 2, 3} {
		id := "L" + strconv.FormatInt(acct, 10)
		if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: acct, BrokerExecID: id, ImportMethod: "motivewave_csv",
			Symbol: "ESZ6", SymbolRoot: "ES", Side: "buy", Qty: 1, Price: 7000, ExecTime: opened.Add(time.Duration(acct) * time.Second)}); err != nil {
			t.Fatal(err)
		}
		e, _ := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{AccountID: acct, BrokerExecID: id})
		stop := stops[acct]
		if _, err := q.CreatePositionPlan(ctx, models.CreatePositionPlanParams{AccountID: acct, Symbol: "ESZ6", OpenExecID: e.ID, OpenedAt: e.ExecTime, Stop: &stop, Source: "trader"}); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{AccountID: 1, BrokerExecID: "L1"})
	s.LastClose = func(context.Context, string) (float64, time.Time, bool) { return 6996.5, at(14, 6, 0), true }
	s.Now = func() time.Time { return at(14, 6, 30) }
	got, _ := s.Check(ctx, uid, false)
	var hits []models.Nudge
	for _, n := range got {
		if n.Key == "past-stop-live" {
			hits = append(hits, n)
		}
	}
	if len(hits) != 2 {
		t.Fatalf("through-stop nudges = %d, want one for the shared stop and one for gamma: %+v", len(hits), hits)
	}
	if hits[0].Ref != strconv.FormatInt(first.ID, 10) || !strings.Contains(hits[0].Detail, "stop 6997 on long on 2 accounts") || !strings.Contains(hits[0].Href, "sheet=pos%3A1%3AESZ6") {
		t.Errorf("shared-stop nudge = %+v", hits[0])
	}
	if !strings.Contains(hits[1].Detail, "stop 6998 on long 1") || !strings.Contains(hits[1].Href, "sheet=pos%3A3%3AESZ6") {
		t.Errorf("own-stop nudge = %+v", hits[1])
	}
}
