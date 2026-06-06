package marketdata

import (
	"testing"
	"time"
)

func TestSelectTimeframe(t *testing.T) {
	now := time.Date(2026, 6, 6, 18, 0, 0, 0, time.UTC)
	mins := func(n int) time.Duration { return time.Duration(n) * time.Minute }
	cases := []struct {
		name   string
		ago    time.Duration // firstEntry = now - ago
		span   time.Duration
		wantTf Timeframe
	}{
		{"8m scalp today", mins(60), mins(8), TF1m},
		{"35m trade today", mins(120), mins(35), TF1m},
		{"2h trade today", mins(180), 2 * time.Hour, TF5m},
		{"2h trade 75d ago degrades", 75 * 24 * time.Hour, 2 * time.Hour, TF1h},
		{"1.5d swing", 3 * 24 * time.Hour, 36 * time.Hour, TF1h},
		{"open/instant floors to 1m", mins(30), 0, TF1m},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entry := now.Add(-c.ago)
			exit := entry.Add(c.span)
			tf, from, to := SelectTimeframe(entry, exit, now)
			if tf != c.wantTf {
				t.Errorf("tf = %s, want %s", tf, c.wantTf)
			}
			if !from.Before(entry) || !to.After(exit) {
				t.Errorf("window [%v,%v] must bracket the trade [%v,%v]", from, to, entry, exit)
			}
			if now.Sub(from) > tfTable[tf].maxLookback {
				t.Errorf("window start %v exceeds %s lookback %v", from, tf, tfTable[tf].maxLookback)
			}
		})
	}
}

func TestAggregateTo(t *testing.T) {
	// 10 one-minute bars starting at a 5m-aligned epoch second.
	base := int64(1_700_000_100) // % 300 == 0
	if base%300 != 0 {
		base -= base % 300
	}
	var oneMin []Bar
	for i := 0; i < 10; i++ {
		oneMin = append(oneMin, Bar{
			Time:   base + int64(i)*60,
			Open:   float64(i),
			High:   float64(i) + 0.5,
			Low:    float64(i) - 0.5,
			Close:  float64(i) + 0.2,
			Volume: 1,
		})
	}
	got := AggregateTo(oneMin, TF5m)
	if len(got) != 2 {
		t.Fatalf("got %d 5m bars, want 2", len(got))
	}
	// First bucket = bars 0..4.
	b0 := got[0]
	if b0.Time != base {
		t.Errorf("bucket0 time = %d, want %d (first sub-bar)", b0.Time, base)
	}
	if b0.Open != 0 {
		t.Errorf("bucket0 open = %v, want 0", b0.Open)
	}
	if b0.Close != 4.2 {
		t.Errorf("bucket0 close = %v, want 4.2", b0.Close)
	}
	if b0.High != 4.5 {
		t.Errorf("bucket0 high = %v, want 4.5", b0.High)
	}
	if b0.Low != -0.5 {
		t.Errorf("bucket0 low = %v, want -0.5", b0.Low)
	}
	if b0.Volume != 5 {
		t.Errorf("bucket0 volume = %v, want 5", b0.Volume)
	}
	if got[1].Open != 5 {
		t.Errorf("bucket1 open = %v, want 5", got[1].Open)
	}
}

func TestAggregateNoOp(t *testing.T) {
	bars := []Bar{{Time: 300, Open: 1, High: 2, Low: 0, Close: 1, Volume: 3}}
	got := AggregateTo(bars, TF5m)
	if len(got) != 1 || got[0] != bars[0] {
		t.Errorf("single-bar aggregation should be a no-op, got %+v", got)
	}
}

func TestSymbolMap(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"ES":   {"ES=F", true},
		"MES":  {"ES=F", true},
		"mnq":  {"NQ=F", true},
		" CL ": {"CL=F", true},
		"ZZ":   {"", false},
	}
	for in, exp := range cases {
		got, ok := YahooSymbol(in)
		if got != exp.want || ok != exp.ok {
			t.Errorf("YahooSymbol(%q) = (%q,%v), want (%q,%v)", in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestStorageTimeframe(t *testing.T) {
	now := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want Timeframe
	}{
		{2 * 24 * time.Hour, TF1m},
		{30 * 24 * time.Hour, TF5m},
		{100 * 24 * time.Hour, TF1h},
		{3 * 365 * 24 * time.Hour, TF1d},
	}
	for _, c := range cases {
		if got := StorageTimeframe(now.Add(-c.ago), now); got != c.want {
			t.Errorf("StorageTimeframe(age=%v) = %s, want %s", c.ago, got, c.want)
		}
	}
}

func TestCoarser(t *testing.T) {
	if Coarser(TF5m, TF1h) != TF1h {
		t.Error("Coarser(5m,1h) should be 1h")
	}
	if Coarser(TF1m, "") != TF1m {
		t.Error("Coarser(1m,\"\") should be 1m")
	}
}
