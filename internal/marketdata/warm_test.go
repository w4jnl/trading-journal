package marketdata

import (
	"context"
	"testing"
	"time"
)

// The warm-up fetches today for every traded root on each tick (whatever
// the TTL says) and completes the previous days once; a root with no
// recent fill is left alone; the newest cached bar is readable without a
// fetch.
func TestWarmer(t *testing.T) {
	db := testDB(t)
	now := time.Now().UTC().Truncate(time.Minute)
	var bars []Bar
	for d := 3; d >= 0; d-- {
		day := startOfDay(now.AddDate(0, 0, -d), time.UTC)
		bars = append(bars, dayBars(day, 300)...)
	}
	src := &fakeSource{bars: bars}
	cache := &CachingSource{DB: db, Source: src, Loc: time.UTC, IntradayTTL: time.Hour}
	// Fills on ES in the last days, and on CL ten days ago (not warm).
	for _, f := range []struct {
		root string
		when time.Time
	}{{"MES", now.Add(-2 * time.Hour)}, {"ES", now.AddDate(0, 0, -1)}, {"CL", now.AddDate(0, 0, -10)}} {
		if _, err := db.Exec(`INSERT INTO executions (account_id, broker_exec_id, symbol, symbol_root, side, qty, price, exec_time)
			VALUES (1, ?, ?, ?, 'buy', 1, 100, ?)`, f.root+f.when.Format(time.RFC3339Nano), f.root+"Z6", f.root, f.when); err != nil {
			t.Fatal(err)
		}
	}
	w := &Warmer{Cache: cache, DB: db, Chicago: time.UTC, Every: time.Minute}
	syms, err := w.symbols(context.Background())
	if err != nil || len(syms) != 1 || syms[0] != "ES=F" {
		t.Fatalf("symbols = %v (%v), want [ES=F]: MES folds into ES, CL is too old", syms, err)
	}
	w.Tick(context.Background())
	// Today plus two past days.
	if src.calls != 3 {
		t.Errorf("first tick fetched %d days, want 3", src.calls)
	}
	w.Tick(context.Background())
	if src.calls != 4 {
		t.Errorf("second tick fetched %d more, want 1 (today only, past days cached)", src.calls-3)
	}
	b, ok := cache.CachedLatest(context.Background(), "ES=F")
	if !ok || b.Close != 100.5 {
		t.Errorf("cached latest = %+v %v", b, ok)
	}
	if _, ok := cache.CachedLatest(context.Background(), "CL=F"); ok {
		t.Errorf("CL was never warmed")
	}
	recent, tf, err := cache.RecentBars(context.Background(), "ES=F", 3, now)
	if err != nil || tf != TF1m || len(recent) == 0 {
		t.Errorf("recent bars = %d at %s (%v)", len(recent), tf, err)
	}
}
