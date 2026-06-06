package marketdata

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE price_bars (yahoo_symbol TEXT, timeframe TEXT, bar_time INTEGER,
			open REAL, high REAL, low REAL, close REAL, volume REAL, fetched_at INTEGER,
			PRIMARY KEY (yahoo_symbol, timeframe, bar_time))`,
		`CREATE TABLE price_bar_days (yahoo_symbol TEXT, timeframe TEXT, day TEXT,
			fetched_at INTEGER, PRIMARY KEY (yahoo_symbol, timeframe, day))`,
		`CREATE TABLE trades (id INTEGER PRIMARY KEY, account_id INTEGER, symbol_root TEXT,
			entry_time DATETIME NOT NULL, exit_time DATETIME)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
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

	// Today: bars stored but never permanently covered (forming session).
	if err := c.FetchDay(ctx, "ES=F", today); err != nil {
		t.Fatalf("FetchDay today: %v", err)
	}
	if present, _ := c.dayPresent(ctx, "ES=F", today); present {
		t.Error("today must not be marked covered")
	}
	if cov, _ := c.WindowCovered(ctx, "ES=F", today.Add(time.Hour), today.Add(2*time.Hour)); cov {
		t.Error("a window including today must not be covered")
	}
}

func TestPrefetcherSkipsCovered(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now()
	loc := time.UTC
	pastDay := startOfDay(now.AddDate(0, 0, -3), loc)

	if _, err := db.Exec(
		`INSERT INTO trades (account_id, symbol_root, entry_time, exit_time) VALUES (1,'ES',?,?)`,
		pastDay.Add(10*time.Hour), pastDay.Add(11*time.Hour)); err != nil {
		t.Fatalf("insert trade: %v", err)
	}

	fake := &fakeSource{bars: dayBars(pastDay, 30)}
	c := &CachingSource{DB: db, Source: fake, Loc: loc}
	p := NewPrefetcher(c, db, loc)
	p.delay = 0 // no throttle in tests

	// Trade spans [entry_day-1 .. exit_day+1] = 3 past days for ES=F.
	p.processAccount(ctx, 1)
	if fake.calls != 3 {
		t.Fatalf("first pass fetched %d days, want 3", fake.calls)
	}
	// Second pass: everything already covered -> no new fetches.
	p.processAccount(ctx, 1)
	if fake.calls != 3 {
		t.Errorf("second pass refetched: %d calls, want 3", fake.calls)
	}
}
