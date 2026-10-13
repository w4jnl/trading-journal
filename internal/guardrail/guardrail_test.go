package guardrail

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
)

func i64(v int64) *int64       { return &v }
func f64(v float64) *float64   { return &v }
func str(v string) *string     { return &v }
func at(h, m, s int) time.Time { return time.Date(2026, 10, 14, h, m, s, 0, time.UTC) }

func TestFromPlan(t *testing.T) {
	trades := []dayTrade{
		{ID: 1, AccountID: 1, Account: "alpha", Entry: at(14, 0, 0), Exit: at(14, 2, 0), Net: -50},
		{ID: 2, AccountID: 2, Account: "bravo", Entry: at(14, 0, 1), Exit: at(14, 2, 1), Net: -90},
		{ID: 3, AccountID: 2, Account: "bravo", Entry: at(14, 5, 0), Exit: at(14, 6, 0), Net: -20},
	}
	plan := &dayplan.Plan{MaxTrades: i64(2), MaxLoss: f64(100)}
	got := fromPlan(plan, 2, trades, at(14, 7, 0), DefaultFresh)
	keys := []string{}
	for _, n := range got {
		keys = append(keys, n.Key+":"+n.Ref)
	}
	// alpha is down 50 (not near 80), bravo 110 (past 100).
	if strings.Join(keys, " ") != "cap-reached:2 max-loss-hit:2" {
		t.Errorf("nudges = %v", keys)
	}
	if got[1].Title != "Max loss reached on bravo" || !strings.Contains(got[1].Detail, "Down 110.00 today against the plan's 100.00") {
		t.Errorf("max loss nudge: %+v", got[1])
	}
	trades[0].Net = -85
	got = fromPlan(plan, 3, trades, at(14, 7, 0), DefaultFresh)
	if len(got) != 2 || got[0].Key != "max-loss-near" || got[0].Ref != "1" || got[1].Key != "max-loss-hit" {
		t.Errorf("past the cap the coach speaks; near on alpha, hit on bravo: %+v", got)
	}
	if got := fromPlan(plan, 2, trades, at(15, 0, 0), DefaultFresh); len(got) != 0 {
		t.Errorf("an hour later nothing is fresh: %+v", got)
	}
	if got := fromPlan(nil, 2, trades, at(14, 7, 0), DefaultFresh); got != nil {
		t.Errorf("no plan, no plan nudges: %+v", got)
	}
}

func TestStopTime(t *testing.T) {
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	plan := &dayplan.Plan{StopTime: "15:30"}
	for _, c := range []struct {
		now  time.Time
		want int
	}{{at(15, 29, 0), 0}, {at(15, 30, 0), 1}, {at(15, 45, 0), 1}, {at(15, 51, 0), 0}} {
		if got := stopTime(plan, day, c.now, DefaultFresh); len(got) != c.want {
			t.Errorf("at %s: %d nudges, want %d", c.now.Format("15:04"), len(got), c.want)
		}
	}
	if got := stopTime(&dayplan.Plan{}, day, at(15, 30, 0), DefaultFresh); got != nil {
		t.Errorf("no stop time: %+v", got)
	}
}

// guardFixture: a user with the guardrail on, a plan (2 decisions, 100 max
// loss, done by 14:10), an open lesson about revenge entries, and two ES
// losers on one account, the second opened 30 s after the first closed.
func guardFixture(t *testing.T) (*Service, *sql.DB, int64) {
	t.Helper()
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	q := models.New(conn)
	ctx := context.Background()
	if err := q.SetUserGuardrail(ctx, models.SetUserGuardrailParams{Guardrail: 1, ID: uid}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ?, 'alpha', 'motivewave_csv', 'A')`, uid); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []struct {
		id          int64
		entry, exit time.Time
		net         float64
	}{{10, at(14, 0, 0), at(14, 2, 0), -80}, {11, at(14, 2, 30), at(14, 5, 0), -40}} {
		if _, err := conn.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl)
			VALUES (?, 1, 'ESZ6', 'ES', 'long', 1, 7000, 6999, ?, ?, 'closed', ?)`, tr.id, tr.entry, tr.exit, tr.net); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dayplan.Save(ctx, q, uid, dayplan.Plan{Day: "2026-10-14", MaxTrades: i64(2), MaxLoss: f64(100), StopTime: "14:10"}, at(8, 0, 0)); err != nil {
		t.Fatal(err)
	}
	for _, l := range []struct{ day, key, text string }{
		{"2026-10-10", "revenge", "Walk away for five minutes after a loss."},
		{"2026-10-12", "chased", "Wait for the retest."},
		{"2026-10-13", "chased", "Wait for the retest, again."},
	} {
		if err := q.InsertLesson(ctx, models.InsertLessonParams{UserID: uid, Day: l.day, Scope: "trade", MistakeKey: str(l.key), Text: l.text}); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{DB: conn, Live: livedata.NewNotifier(), TZ: time.UTC}, conn, uid
}

// A live day: the re-entry after a loss, the cap reached and the max loss
// hit are each said once, quoting the trader's lessons; the stop time is
// said when it comes; a quiet tick says nothing new.
func TestCheckNudgesOnce(t *testing.T) {
	s, conn, uid := guardFixture(t)
	ctx := context.Background()
	now := at(14, 6, 0)
	s.Now = func() time.Time { return now }

	got, err := s.Check(ctx, uid, true)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]models.Nudge{}
	for _, n := range got {
		byKey[n.Key] = n
	}
	if len(got) != 3 || byKey["revenge-entry"].Title == "" || byKey["cap-reached"].Title == "" || byKey["max-loss-hit"].Title == "" {
		t.Fatalf("nudges: %+v", got)
	}
	if l := byKey["revenge-entry"].Lesson; l == nil || *l != "Walk away for five minutes after a loss." {
		t.Errorf("the re-entry nudge should quote the revenge lesson: %v", l)
	}
	if l := byKey["cap-reached"].Lesson; l == nil || *l != "Wait for the retest, again." {
		t.Errorf("a plan nudge with no lesson of its own quotes the most repeated one: %v", l)
	}
	if v := s.Live.Version(uid); v != 1 {
		t.Errorf("new nudges should bump the page version once, got %d", v)
	}
	if again, _ := s.Check(ctx, uid, true); len(again) != 0 {
		t.Errorf("said twice: %+v", again)
	}

	// The stop time on the clock, without new data.
	now = at(14, 11, 0)
	got, _ = s.Check(ctx, uid, false)
	if len(got) != 1 || got[0].Key != "stop-time" || !strings.Contains(got[0].Detail, "It is 14:10") {
		t.Errorf("stop time: %+v", got)
	}

	var n int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM nudges WHERE user_id = ? AND day = '2026-10-14'`, uid).Scan(&n)
	if n != 4 {
		t.Errorf("%d nudges stored, want 4", n)
	}
}

// Tick reads a user's day when their data moved (and on the first look),
// and only for users who turned the guardrail on.
func TestTick(t *testing.T) {
	s, conn, uid := guardFixture(t)
	ctx := context.Background()
	now := at(13, 0, 0) // before the trades: nothing to say
	s.Now = func() time.Time { return now }
	s.Tick(ctx)
	count := func() (n int) {
		_ = conn.QueryRow(`SELECT COUNT(*) FROM nudges`).Scan(&n)
		return
	}
	if count() != 0 {
		t.Fatalf("nothing should be said before the trades")
	}
	now = at(14, 6, 0)
	s.Tick(ctx) // data unchanged since the last look: the clock only
	if count() != 0 {
		t.Fatalf("an unchanged day was read again")
	}
	s.Live.Bump(uid)
	s.Tick(ctx)
	if count() != 3 {
		t.Fatalf("after new data: %d nudges, want 3", count())
	}
	if _, err := conn.Exec(`UPDATE users SET guardrail = 0`); err != nil {
		t.Fatal(err)
	}
	now = at(14, 11, 0)
	s.Tick(ctx)
	if count() != 3 {
		t.Errorf("a user who turned it off was nudged")
	}
}
