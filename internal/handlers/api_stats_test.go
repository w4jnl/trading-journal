package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

func TestParseAPIDateRange(t *testing.T) {
	tz, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata")
	}
	today := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC) // 08:00 in Chicago, same date
	day := func(tm time.Time) string { return tm.Format("2006-01-02") }

	// Defaults: to = today, from = to - 29 days (30 inclusive days), in tz.
	rng, err := parseAPIDateRange("", "", tz, today)
	if err != nil {
		t.Fatal(err)
	}
	if day(rng.From) != "2026-08-04" || day(rng.To) != "2026-09-02" || rng.Days() != 30 {
		t.Errorf("defaults = %s..%s (%d days)", day(rng.From), day(rng.To), rng.Days())
	}
	if rng.From.Location() != tz {
		t.Errorf("range not in the trading tz")
	}
	if rng.ToExclusive() != rng.To.AddDate(0, 0, 1) {
		t.Errorf("ToExclusive = %v", rng.ToExclusive())
	}

	// One-sided ranges fill the other bound from the default rule.
	rng, _ = parseAPIDateRange("2026-08-01", "", tz, today)
	if day(rng.From) != "2026-08-01" || day(rng.To) != "2026-09-02" {
		t.Errorf("from-only = %s..%s", day(rng.From), day(rng.To))
	}
	rng, _ = parseAPIDateRange("", "2026-08-31", tz, today)
	if day(rng.From) != "2026-08-02" || day(rng.To) != "2026-08-31" {
		t.Errorf("to-only = %s..%s", day(rng.From), day(rng.To))
	}

	// A DST month (Chicago springs forward Mar 8) still counts 31 days.
	rng, _ = parseAPIDateRange("2026-03-01", "2026-03-31", tz, today)
	if rng.Days() != 31 {
		t.Errorf("DST month days = %d", rng.Days())
	}

	// Span cap: exactly 400 inclusive days pass, 401 fail.
	if _, err := parseAPIDateRange("2025-07-28", "2026-08-31", tz, today); err != nil {
		t.Errorf("400-day span rejected: %v", err)
	}
	if _, err := parseAPIDateRange("2025-07-27", "2026-08-31", tz, today); err == nil {
		t.Errorf("401-day span accepted")
	}

	// Junk and inverted ranges are validation errors (-> 400), never a silent
	// live/default view.
	for _, tc := range [][2]string{{"nope", ""}, {"", "2026-13-01"}, {"2026/08/01", ""}, {"2026-09-02", "2026-09-01"}} {
		_, err := parseAPIDateRange(tc[0], tc[1], tz, today)
		var ve apiValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%v: err = %v, want validation error", tc, err)
		}
	}
}

// The per-day merge is the Calendar's: admitted accounts summed per
// trading-TZ date, extremes folded, breakevens in neither bucket.
func TestAggregateDailyStats(t *testing.T) {
	tz, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata")
	}
	d := func(day int) time.Time { return time.Date(2026, 8, day, 0, 0, 0, 0, tz) }
	rows := []models.DailyStat{
		{AccountID: 1, StatDate: d(4), TradesCount: 2, Wins: 1, Losses: 1, NetPnl: 40, GrossPnl: 48, Commission: 8, LargestWin: ptrF(70), LargestLoss: ptrF(-30)},
		{AccountID: 1, StatDate: d(3), TradesCount: 3, Wins: 2, Losses: 1, NetPnl: 100, GrossPnl: 112, Commission: 12, LargestWin: ptrF(80), LargestLoss: ptrF(-30)},
		// Same day, second account, as a scanned row comes back (fixed offset,
		// not the named zone); no losing trade -> largestLoss stays nil.
		{AccountID: 2, StatDate: time.Date(2026, 8, 3, 0, 0, 0, 0, time.FixedZone("CDT", -5*3600)), TradesCount: 1, Wins: 1, NetPnl: 50, GrossPnl: 52, Commission: 2, LargestWin: ptrF(50)},
		{AccountID: 3, StatDate: d(3), TradesCount: 9, Wins: 9, NetPnl: 999, LargestWin: ptrF(999)},  // retired
		{AccountID: 4, StatDate: d(5), TradesCount: 1, Losses: 1, NetPnl: -5, LargestLoss: ptrF(-5)}, // excluded from stats
		{AccountID: 1, StatDate: d(6), TradesCount: 2},                                               // breakeven-only day
	}
	retired := map[int64]bool{3: true}
	excluded := map[int64]bool{4: true}

	// Default rule: retired out, excluded out, merged per day, ascending.
	items := aggregateDailyStats(rows, newAccountAdmission(accountFilter{}, retired, excluded).admits, tz)
	if len(items) != 3 || items[0].Date != "2026-08-03" || items[1].Date != "2026-08-04" || items[2].Date != "2026-08-06" {
		t.Fatalf("items = %+v", items)
	}
	aug3 := items[0]
	if aug3.Trades != 4 || aug3.Wins != 3 || aug3.Losses != 1 || aug3.Net != 150 || aug3.Gross != 164 || aug3.Commission != 14 {
		t.Errorf("aug 3 merge = %+v", aug3)
	}
	if aug3.LargestWin == nil || *aug3.LargestWin != 80 || aug3.LargestLoss == nil || *aug3.LargestLoss != -30 {
		t.Errorf("aug 3 largest = %v / %v", aug3.LargestWin, aug3.LargestLoss)
	}
	if aug3.WinRate == nil || *aug3.WinRate != 0.75 {
		t.Errorf("aug 3 winRate = %v", aug3.WinRate)
	}
	// Extremes are copies, never pointers into the source rows.
	*rows[1].LargestWin = 1
	if *items[0].LargestWin != 80 {
		t.Errorf("largestWin aliases the source row")
	}
	if be := items[2]; be.WinRate != nil || be.LargestWin != nil || be.LargestLoss != nil || be.Trades != 2 {
		t.Errorf("breakeven day = %+v", be)
	}

	// An explicit filter admits the excluded account - and nothing else.
	items = aggregateDailyStats(rows, newAccountAdmission(accountFilter{Active: true, AccountIDs: []int64{4}}, retired, excluded).admits, tz)
	if len(items) != 1 || items[0].Date != "2026-08-05" || items[0].Net != -5 {
		t.Errorf("filtered = %+v", items)
	}
	// Naming a retired account still yields nothing; an empty resolved filter
	// shows none (never everything).
	if n := len(aggregateDailyStats(rows, newAccountAdmission(accountFilter{Active: true, AccountIDs: []int64{3}}, retired, excluded).admits, tz)); n != 0 {
		t.Errorf("retired admitted via filter: %d items", n)
	}
	if n := len(aggregateDailyStats(rows, newAccountAdmission(accountFilter{Active: true}, retired, excluded).admits, tz)); n != 0 {
		t.Errorf("empty filter showed %d items", n)
	}
	// Empty input is an empty, non-nil slice (JSON [] not null).
	if got := aggregateDailyStats(nil, func(int64) bool { return true }, tz); got == nil || len(got) != 0 {
		t.Errorf("empty input = %#v", got)
	}
}

func TestDailyStatsTotals(t *testing.T) {
	items := []apiDailyStatsItem{
		{Date: "2026-08-03", apiSummary: apiSummary{Trades: 4, Wins: 3, Losses: 1, Net: 150, Gross: 164, Commission: 14}, LargestWin: ptrF(80), LargestLoss: ptrF(-30)},
		{Date: "2026-08-04", apiSummary: apiSummary{Trades: 2, Wins: 1, Losses: 1, Net: -60, Gross: -52, Commission: 8}, LargestWin: ptrF(10), LargestLoss: ptrF(-70)},
		{Date: "2026-08-06", apiSummary: apiSummary{Trades: 2}}, // breakeven day: neither a win nor a loss day
	}
	tot := dailyStatsTotals(items)
	if tot.Trades != 8 || tot.Wins != 4 || tot.Losses != 2 || tot.Net != 90 || tot.Gross != 112 || tot.Commission != 22 {
		t.Errorf("sums = %+v", tot.apiSummary)
	}
	if tot.WinDays != 1 || tot.LossDays != 1 {
		t.Errorf("win/loss days = %d/%d", tot.WinDays, tot.LossDays)
	}
	if tot.BestDay == nil || tot.BestDay.Date != "2026-08-03" || tot.BestDay.Net != 150 ||
		tot.WorstDay == nil || tot.WorstDay.Date != "2026-08-04" || tot.WorstDay.Net != -60 {
		t.Errorf("best/worst = %+v / %+v", tot.BestDay, tot.WorstDay)
	}
	if tot.LargestWin == nil || *tot.LargestWin != 80 || tot.LargestLoss == nil || *tot.LargestLoss != -70 {
		t.Errorf("largest = %v / %v", tot.LargestWin, tot.LargestLoss)
	}
	if tot.WinRate == nil || *tot.WinRate != 4.0/6.0 {
		t.Errorf("winRate = %v", tot.WinRate)
	}
	empty := dailyStatsTotals(nil)
	if empty.Trades != 0 || empty.WinRate != nil || empty.BestDay != nil || empty.WorstDay != nil || empty.LargestWin != nil || empty.LargestLoss != nil {
		t.Errorf("empty totals = %+v", empty)
	}
}

// The projection is camelCase-only (no sqlc column names leak) and an empty
// result serializes as [] / null exactly as the contract promises.
func TestApiDailyStatsJSON(t *testing.T) {
	out := apiDailyStats{From: "2026-08-03", To: "2026-08-03", Timezone: "UTC",
		Totals: dailyStatsTotals(nil), Items: make([]apiDailyStatsItem, 0)}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"items":[]`, `"winRate":null`, `"bestDay":null`, `"worstDay":null`, `"largestWin":null`, `"winDays":0`, `"trades":0`, `"timezone":"UTC"`} {
		if !strings.Contains(s, want) {
			t.Errorf("json missing %s: %s", want, s)
		}
	}
	for _, not := range []string{"stat_date", "trades_count", "net_pnl", "largest_win", "account_id"} {
		if strings.Contains(s, not) {
			t.Errorf("snake_case leaked: %s", not)
		}
	}
	// The embedded summary flattens into the same six names the journal
	// endpoints use, right after date.
	b, _ = json.Marshal(apiDailyStatsItem{Date: "2026-08-03", apiSummary: apiSummary{Trades: 1, Net: 5}})
	if !strings.Contains(string(b), `"date":"2026-08-03","trades":1,"net":5,"gross":0,"commission":0,"wins":0,"losses":0,"winRate":null`) {
		t.Errorf("item json = %s", b)
	}
}

// End to end through the API mux with a real DB: the Calendar account rule,
// the filter forms, user scoping (IDOR), empty ranges and the 400s.
func TestAPIStatsDailyHandler(t *testing.T) {
	conn, q := newAuthzTestDB(t)
	ctx := context.Background()
	alice, err := q.CreateUser(ctx, models.CreateUserParams{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := q.CreateUser(ctx, models.CreateUserParams{Username: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	live, eval := "live", "prop_eval"
	mk := func(uid int64, name string, typ *string) models.Account {
		a, err := q.CreateAccount(ctx, models.CreateAccountParams{
			UserID: uid, Name: name, ImportSource: "motivewave_csv", AccountRef: name,
			Type: typ, Currency: "USD", CommissionSource: "rate",
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return a
	}
	a1 := mk(alice.ID, "A-live", &live)
	a2 := mk(alice.ID, "A-eval", &eval) // excluded from stats
	a3 := mk(alice.ID, "A-old", &live)  // retired
	b1 := mk(bob.ID, "B-live", &live)
	if err := q.SetAccountExcluded(ctx, models.SetAccountExcludedParams{ExcludedFromStats: 1, ID: a2.ID, UserID: alice.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE accounts SET active = 0 WHERE id = ?", a3.ID); err != nil {
		t.Fatal(err)
	}

	tz := time.UTC
	day := func(dd int) time.Time { return time.Date(2026, 8, dd, 0, 0, 0, 0, tz) }
	seed := func(acc int64, dd int, trades, wins, losses int64, net float64, lw, ll *float64) {
		if err := q.CreateDailyStat(ctx, models.CreateDailyStatParams{
			AccountID: acc, StatDate: day(dd), TradesCount: trades, Wins: wins, Losses: losses,
			GrossPnl: net + 2, NetPnl: net, Commission: 2, LargestWin: lw, LargestLoss: ll,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed(a1.ID, 3, 3, 2, 1, 100, ptrF(80), ptrF(-30))
	seed(a1.ID, 4, 1, 0, 1, -20, nil, ptrF(-20))
	seed(a2.ID, 3, 5, 5, 0, 500, ptrF(200), nil) // excluded by default
	seed(a3.ID, 3, 7, 7, 0, 700, ptrF(300), nil) // retired: never
	seed(b1.ID, 3, 9, 9, 0, 900, ptrF(400), nil) // bob's

	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: tz}
	get := func(uid int64, target string) (*httptest.ResponseRecorder, apiDailyStats) {
		req := httptest.NewRequest("GET", target, nil)
		req = req.WithContext(auth.WithUser(req.Context(), &auth.User{ID: uid, Username: "u"}))
		rec := httptest.NewRecorder()
		h.APIMux().ServeHTTP(rec, req)
		var out apiDailyStats
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("%s: bad json: %v", target, err)
			}
		}
		return rec, out
	}
	base := "/api/v1/stats/daily?from=2026-08-01&to=2026-08-31"

	// Default: alice's live account only - excluded + retired dropped, bob invisible.
	rec, out := get(alice.ID, base)
	if rec.Code != http.StatusOK {
		t.Fatalf("default = %d: %s", rec.Code, rec.Body.String())
	}
	if len(out.Items) != 2 || out.Items[0].Date != "2026-08-03" || out.Items[0].Trades != 3 || out.Items[0].Net != 100 || out.Items[1].Date != "2026-08-04" {
		t.Errorf("default items = %+v", out.Items)
	}
	if out.Totals.Trades != 4 || out.Totals.Net != 80 || out.Totals.WinDays != 1 || out.Totals.LossDays != 1 ||
		out.From != "2026-08-01" || out.To != "2026-08-31" || out.Timezone != "UTC" {
		t.Errorf("default totals = %+v (%s..%s %s)", out.Totals, out.From, out.To, out.Timezone)
	}
	if out.Totals.LargestWin == nil || *out.Totals.LargestWin != 80 || out.Totals.LargestLoss == nil || *out.Totals.LargestLoss != -30 {
		t.Errorf("default largest = %v / %v", out.Totals.LargestWin, out.Totals.LargestLoss)
	}

	// An explicit account filter overrides excluded_from_stats.
	if _, out := get(alice.ID, fmt.Sprintf("%s&filter=acc:%d", base, a2.ID)); len(out.Items) != 1 || out.Items[0].Trades != 5 || out.Items[0].Net != 500 {
		t.Errorf("acc filter = %+v", out.Items)
	}
	// type: resolves across ALL users' accounts (not user-scoped), but the
	// user-scoped query means only alice's live, non-retired account counts.
	if _, out := get(alice.ID, base+"&filter=type:live"); out.Totals.Trades != 4 {
		t.Errorf("type filter totals = %+v", out.Totals)
	}
	// Legacy account_id bridge.
	if _, out := get(alice.ID, fmt.Sprintf("%s&account_id=%d", base, a2.ID)); out.Totals.Trades != 5 {
		t.Errorf("legacy account_id = %+v", out.Totals)
	}
	// IDOR: naming bob's account yields nothing - never his rows.
	if _, out := get(alice.ID, fmt.Sprintf("%s&filter=acc:%d", base, b1.ID)); len(out.Items) != 0 || out.Totals.Trades != 0 || out.Totals.WinRate != nil {
		t.Errorf("cross-user filter leaked: %+v", out)
	}
	if _, out := get(bob.ID, base); out.Totals.Trades != 9 {
		t.Errorf("bob totals = %+v", out.Totals)
	}
	// Empty range: 200 with items [] (not null) and null extremes.
	if rec, _ := get(alice.ID, "/api/v1/stats/daily?from=2026-01-01&to=2026-01-31"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"items":[]`) || !strings.Contains(rec.Body.String(), `"bestDay":null`) {
		t.Errorf("empty range = %d %s", rec.Code, rec.Body.String())
	}
	// Bad input is a 400 with the error envelope.
	for _, bad := range []string{"?from=nope", "?from=2026-08-31&to=2026-08-01", "?from=2020-01-01&to=2026-08-31"} {
		if rec, _ := get(alice.ID, "/api/v1/stats/daily"+bad); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s = %d %s", bad, rec.Code, rec.Body.String())
		}
	}
}

// /api/v1/version is the key-authenticated twin of /data/version: the
// per-user counter, no-store, nil-safe.
func TestAPIVersionRoute(t *testing.T) {
	n := livedata.NewNotifier()
	h := &Handlers{LiveData: n}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/version", nil)
		req = req.WithContext(auth.WithUser(req.Context(), &auth.User{ID: 7, Username: "t"}))
		rec := httptest.NewRecorder()
		h.APIMux().ServeHTTP(rec, req)
		return rec
	}
	rec := get()
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"v":0}` || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("initial = %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	n.Bump(7)
	if rec := get(); strings.TrimSpace(rec.Body.String()) != `{"v":1}` {
		t.Errorf("after bump = %q", rec.Body.String())
	}
	n.Bump(8) // another user's change is invisible
	if rec := get(); strings.TrimSpace(rec.Body.String()) != `{"v":1}` {
		t.Errorf("other user's bump leaked: %q", rec.Body.String())
	}
	h.LiveData = nil
	if rec := get(); rec.Code != http.StatusOK {
		t.Errorf("nil notifier = %d", rec.Code)
	}
}
