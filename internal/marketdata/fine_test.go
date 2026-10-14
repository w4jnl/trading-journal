package marketdata

import (
	"context"
	"testing"
	"time"
)

// fakeExchange serves bars at any size for a window, counting calls.
type fakeExchange struct {
	calls   int
	seconds []int
	err     error
	price   float64
}

func (f *fakeExchange) FetchExchangeBars(_ context.Context, _ string, unitSeconds int, from, to time.Time) ([]Bar, error) {
	f.calls++
	f.seconds = append(f.seconds, unitSeconds)
	if f.err != nil {
		return nil, f.err
	}
	var out []Bar
	step := int64(unitSeconds)
	for t := from.Unix() - from.Unix()%step; t < to.Unix(); t += step {
		out = append(out, Bar{Time: t, Open: f.price, High: f.price + 0.25, Low: f.price - 0.25, Close: f.price, Volume: 3})
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// Today is filled from the exchange feed first and marked so; an old day
// still comes from the vendor; a feed that fails falls back to the vendor.
func TestFetchDayPrefersExchange(t *testing.T) {
	db := testDB(t)
	now := time.Now().UTC()
	today := startOfDay(now, time.UTC)
	vendor := &fakeSource{bars: append(dayBars(today.AddDate(0, 0, -10), 200), dayBars(today, 200)...)}
	ex := &fakeExchange{price: 500}
	cache := &CachingSource{DB: db, Source: vendor, Exchange: ex, Loc: time.UTC, IntradayTTL: time.Hour}
	if err := cache.FetchDay(context.Background(), "ES=F", today); err != nil {
		t.Fatal(err)
	}
	if ex.calls != 1 || ex.seconds[0] != 60 || vendor.calls != 0 {
		t.Errorf("today: exchange calls %d (%v), vendor calls %d", ex.calls, ex.seconds, vendor.calls)
	}
	if src := cache.DaySource(context.Background(), "ES=F", today); src != SourceExchange {
		t.Errorf("today's source = %q", src)
	}
	bars, _, _, _ := cache.BarsForWindow(context.Background(), "ES=F", today, now)
	if len(bars) == 0 || bars[0].Close != 500 {
		t.Errorf("today's bars are not the exchange's: %d bars, first %+v", len(bars), bars[0])
	}
	old := today.AddDate(0, 0, -10)
	if err := cache.FetchDay(context.Background(), "ES=F", old); err != nil {
		t.Fatal(err)
	}
	if ex.calls != 1 || vendor.calls != 1 || cache.DaySource(context.Background(), "ES=F", old) != SourceVendor {
		t.Errorf("an old day must come from the vendor: exchange %d, vendor %d", ex.calls, vendor.calls)
	}
	ex.err = ErrUnsupported
	if err := cache.RefreshToday(context.Background(), "ES=F"); err != nil {
		t.Fatal(err)
	}
	if vendor.calls != 2 || cache.DaySource(context.Background(), "ES=F", today) != SourceVendor {
		t.Errorf("a failing feed must fall back to the vendor (vendor calls %d)", vendor.calls)
	}
}

// One-second bars are a layer of their own: fetched once per window,
// readable when a window covers the stretch, never rolled into a day
// read, and the fetcher serves recent short trades once each.
func TestFineBarsAndFetcher(t *testing.T) {
	db := testDB(t)
	now := time.Now().UTC().Truncate(time.Minute)
	today := startOfDay(now, time.UTC)
	ex := &fakeExchange{price: 600}
	cache := &CachingSource{DB: db, Source: &fakeSource{bars: dayBars(today, 600)}, Exchange: ex, Loc: time.UTC, IntradayTTL: time.Hour}
	if err := cache.FetchDay(context.Background(), "ES=F", today); err != nil {
		t.Fatal(err)
	}
	entry := now.Add(-30 * time.Minute)
	exit := entry.Add(40 * time.Second)
	if _, err := db.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status)
		VALUES (77, 1, 'MESZ6', 'MES', 'long', 2, 600, 600.5, ?, ?, 'closed')`, entry, exit); err != nil {
		t.Fatal(err)
	}
	// A long trade is not served.
	if _, err := db.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status)
		VALUES (78, 1, 'MESZ6', 'MES', 'long', 1, 600, 601, ?, ?, 'closed')`, now.Add(-3*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var served []int64
	ff := &FineFetcher{Cache: cache, DB: db, AfterTrade: func(_ context.Context, tr FineTrade) { served = append(served, tr.ID) }}
	n, err := ff.Run(context.Background())
	if err != nil || n != 1 || len(served) != 1 || served[0] != 77 {
		t.Fatalf("run = %d (%v), served %v", n, err, served)
	}
	if ex.calls != 2 || ex.seconds[1] != 1 {
		t.Errorf("fine fetch calls = %v", ex.seconds)
	}
	fine, ok, err := cache.FineBars(context.Background(), "ES=F", entry.Add(-time.Second), exit.Add(time.Second))
	if err != nil || !ok || len(fine) < 40 || fine[0].Close != 600 {
		t.Errorf("fine bars = %d ok=%v (%v)", len(fine), ok, err)
	}
	if _, ok, _ := cache.FineBars(context.Background(), "ES=F", entry.Add(-time.Hour), exit); ok {
		t.Errorf("a stretch beyond the window must not count as covered")
	}
	// The day read ignores the one-second layer.
	day, tf, _, _ := cache.BarsForWindow(context.Background(), "ES=F", entry.Add(-time.Minute), exit.Add(time.Minute))
	if tf != TF1m || len(day) > 4 {
		t.Errorf("day read = %d bars at %s, want the minute bars only", len(day), tf)
	}
	if n, _ := ff.Run(context.Background()); n != 0 || ex.calls != 2 {
		t.Errorf("second run fetched again: n=%d calls=%d", n, ex.calls)
	}
	// Purging drops the window with the bars.
	if _, err := cache.PurgeIntraday(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cache.FineBars(context.Background(), "ES=F", entry, exit); ok {
		t.Errorf("window survived the purge")
	}
}
