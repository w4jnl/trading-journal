package marketdata

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
)

// fakeSource returns the configured bars that fall within [from,to); empty -> ErrNoData.
type fakeSource struct {
	calls int
	bars  []Bar
	err   error
}

func (f *fakeSource) FetchBars(_ context.Context, _ string, _ Timeframe, from, to time.Time) ([]Bar, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out []Bar
	for _, b := range f.bars {
		if b.Time >= from.Unix() && b.Time < to.Unix() {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// testDB is a migrated database with one account (id 1) for the bootstrap
// user; seedTrade adds closed ES trades to it with the real NOT NULL set.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db := dbtest.Open(t, "mkt")
	uid := dbtest.BootstrapUserID(t, db, "mkt")
	if _, err := db.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ?, 'Acct', 'motivewave_csv', 'A-1')`, uid); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return db
}

func seedTrade(t *testing.T, db *sql.DB, entry, exit time.Time) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO trades (account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status)
		 VALUES (1, 'ESU6', 'ES', 'long', 1, 100, 101, ?, ?, 'closed')`, entry, exit); err != nil {
		t.Fatalf("insert trade: %v", err)
	}
}

func dayBars(day time.Time, n int) []Bar {
	var out []Bar
	for i := 0; i < n; i++ {
		out = append(out, Bar{Time: day.Unix() + int64(i)*60, Open: 100, High: 101, Low: 99, Close: 100.5, Volume: 10})
	}
	return out
}

func TestCacheFetchDayCoverage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now()
	loc := time.UTC
	pastDay := startOfDay(now.AddDate(0, 0, -3), loc)
	today := startOfDay(now, loc)

	fake := &fakeSource{bars: append(dayBars(pastDay, 30), dayBars(today, 30)...)}
	c := &CachingSource{DB: db, Source: fake, Loc: loc}

	// Past day: fetch, store, mark covered.
	if err := c.FetchDay(ctx, "ES=F", pastDay); err != nil {
		t.Fatalf("FetchDay past: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("fake called %d times, want 1", fake.calls)
	}
	if present, _ := c.dayPresent(ctx, "ES=F", pastDay); !present {
		t.Fatal("past day should be marked present")
	}
	covered, err := c.WindowCovered(ctx, "ES=F", pastDay.Add(time.Hour), pastDay.Add(2*time.Hour))
	if err != nil || !covered {
		t.Fatalf("past window covered=%v err=%v, want true", covered, err)
	}
	bars, _, _, err := c.BarsForWindow(ctx, "ES=F", pastDay, pastDay.AddDate(0, 0, 1))
	if err != nil || len(bars) == 0 {
		t.Fatalf("BarsForWindow returned %d bars, err=%v", len(bars), err)
	}

	// Re-ensure the same window: no new fetch (already covered).
	if err := c.EnsureWindow(ctx, "ES=F", pastDay.Add(time.Hour), pastDay.Add(2*time.Hour)); err != nil {
		t.Fatalf("EnsureWindow: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("covered window refetched: fake called %d times, want 1", fake.calls)
	}

	// Today: bars stored AND marked covered while fresh (within IntradayTTL), so
	// the chart renders and reopens hold; once older than the TTL it re-fetches.
	c.IntradayTTL = time.Hour
	if err := c.FetchDay(ctx, "ES=F", today); err != nil {
		t.Fatalf("FetchDay today: %v", err)
	}
	if present, _ := c.dayPresent(ctx, "ES=F", today); !present {
		t.Error("a freshly-fetched today should be marked covered")
	}
	if cov, _ := c.WindowCovered(ctx, "ES=F", today.Add(time.Hour), today.Add(2*time.Hour)); !cov {
		t.Error("a freshly-fetched today window should be covered")
	}
	// A tiny TTL makes the just-fetched copy immediately stale -> not covered.
	c.IntradayTTL = time.Nanosecond
	if cov, _ := c.WindowCovered(ctx, "ES=F", today.Add(time.Hour), today.Add(2*time.Hour)); cov {
		t.Error("a stale today window must not be covered")
	}
}

func TestPrefetcherSkipsCovered(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now()
	loc := time.UTC
	pastDay := startOfDay(now.AddDate(0, 0, -3), loc)

	seedTrade(t, db, pastDay.Add(10*time.Hour), pastDay.Add(11*time.Hour))

	fake := &fakeSource{bars: dayBars(pastDay, 30)}
	c := &CachingSource{DB: db, Source: fake, Loc: loc}
	p := NewPrefetcher(c, db, loc)
	p.delay = 0 // no throttle in tests

	// Trade spans [entry_day-1 .. exit_day+1] = 3 past days for ES=F.
	p.processAccount(ctx, 1, 0)
	if fake.calls != 3 {
		t.Fatalf("first pass fetched %d days, want 3", fake.calls)
	}
	// Second pass: everything already covered -> no new fetches.
	p.processAccount(ctx, 1, 0)
	if fake.calls != 3 {
		t.Errorf("second pass refetched: %d calls, want 3", fake.calls)
	}
}

func TestTradeDaysIncludesToday(t *testing.T) {
	loc := time.UTC
	now := time.Now()
	today := startOfDay(now, loc)
	exit := today.Add(11 * time.Hour)
	days := TradeDays(today.Add(10*time.Hour), &exit, loc, now)
	var hasToday, hasFuture bool
	for _, d := range days {
		if d.Equal(today) {
			hasToday = true
		}
		if d.After(today) {
			hasFuture = true
		}
	}
	if !hasToday {
		t.Error("TradeDays should include today")
	}
	if hasFuture {
		t.Error("TradeDays must not include future days")
	}
}

func TestPrefetcherForceRefetchesRecent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now()
	loc := time.UTC
	recent := startOfDay(now.AddDate(0, 0, -1), loc) // yesterday
	seedTrade(t, db, recent.Add(10*time.Hour), recent.Add(11*time.Hour))
	fake := &fakeSource{bars: dayBars(recent, 30)}
	c := &CachingSource{DB: db, Source: fake, Loc: loc}
	p := NewPrefetcher(c, db, loc)
	p.delay = 0

	p.processAccount(ctx, 1, 0)
	n1 := fake.calls
	if n1 == 0 {
		t.Fatal("first pass fetched nothing")
	}
	// Non-force second pass: recent days are covered/fresh -> nothing re-fetched.
	p.processAccount(ctx, 1, 0)
	if fake.calls != n1 {
		t.Errorf("non-force pass refetched: %d, want %d", fake.calls, n1)
	}
	// Force the last 3 days -> a covered recent day is re-fetched (completes partials).
	p.processAccount(ctx, 1, 3)
	if fake.calls <= n1 {
		t.Errorf("force pass did not re-fetch a covered recent day: calls=%d, was %d", fake.calls, n1)
	}
}
