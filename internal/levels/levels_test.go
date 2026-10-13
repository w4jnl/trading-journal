package levels

import (
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// bars builds 1m bars from [low, high] pairs, closes at the midpoint.
func bars(start time.Time, lh ...[2]float64) []marketdata.Bar {
	out := make([]marketdata.Bar, len(lh))
	for i, p := range lh {
		mid := (p[0] + p[1]) / 2
		out[i] = marketdata.Bar{Time: start.Add(time.Duration(i) * time.Minute).Unix(), Open: mid, High: p[1], Low: p[0], Close: mid}
	}
	return out
}

func TestReplay(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	entry := t0.Add(30 * time.Second).Unix() // inside the first bar
	after := bars(t0,
		[2]float64{6990, 7010}, // the entry's bar: ignored even though it spans both levels
		[2]float64{6998, 7003},
		[2]float64{7002, 7007},
		[2]float64{7005, 7012}) // target 7010 here
	if o := Replay(after, "long", 7000, 6995, 7010, entry); o.Hit != "target" || o.Points != 10 || o.Minutes != 2.5 {
		t.Errorf("target = %+v", o)
	}
	if o := Replay(after, "long", 7000, 6999, 7010, entry); o.Hit != "stop" || o.Points != -1 {
		t.Errorf("stop = %+v", o)
	}
	// The 14:01 bar (6998-7003) reaches both: which came first is unknown,
	// and the result is counted as the stop.
	if o := Replay(after, "long", 7000, 6998.5, 7002.5, entry); o.Hit != "both" || o.Points != -1.5 {
		t.Errorf("both = %+v", o)
	}
	if o := Replay(after, "long", 7000, 6990, 7050, entry); o.Hit != "neither" || o.Points != 8.5 || o.Minutes != 2.5 {
		t.Errorf("neither = %+v", o)
	}
	short := Replay(after, "short", 7006, 7011, 6998, entry)
	if short.Hit != "target" || short.Points != 8 {
		t.Errorf("short = %+v", short)
	}
	if o := Replay(nil, "long", 7000, 6995, 7010, entry); o.Hit != "nodata" {
		t.Errorf("no bars = %+v", o)
	}
}

func TestGridAndBaseline(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	entry := t0.Add(30 * time.Second).Unix()
	up := bars(t0, [2]float64{6999, 7001}, [2]float64{6999, 7003}, [2]float64{7002, 7006})   // runs +6
	down := bars(t0, [2]float64{6999, 7001}, [2]float64{6996, 7000}, [2]float64{6990, 6997}) // falls -10
	ds := []LabDecision{
		{Direction: "long", Entry: 7000, EntryTime: entry, After: up, Actual: 2},
		{Direction: "long", Entry: 7000, EntryTime: entry, After: up, Actual: 1},
		{Direction: "long", Entry: 7000, EntryTime: entry, After: down, Actual: -3},
		{Direction: "long", Entry: 7000, EntryTime: entry, After: bars(t0, [2]float64{6999, 7001})}, // only the entry bar: skipped
	}
	g := Grid(ds, []float64{2, 5}, []float64{4, 0}, 0)
	// stop 2 / target 4: two targets (+4), one stop (-2) -> +2 avg.
	if c := g[0][0]; c.N != 3 || c.Wins != 2 || c.Avg != 2 || c.Total != 6 || c.WinRate != 0.667 {
		t.Errorf("stop 2 / target 4 = %+v", c)
	}
	// stop 5 / no target: the ups run to the last close (+4), the down is stopped (-5).
	if c := g[1][1]; c.N != 3 || c.Total != 3 || c.Unresolved != 2 {
		t.Errorf("stop 5 / no target = %+v", c)
	}
	if c := g[0][0]; c.CILow > c.Avg || c.CIHigh < c.Avg {
		t.Errorf("interval %v..%v does not hold the mean %v", c.CILow, c.CIHigh, c.Avg)
	}
	// A 1-minute time stop closes each at the close of the first bar after
	// the entry: +1, +1 and -2.
	if c := Grid(ds, []float64{20}, []float64{0}, 1)[0][0]; c.N != 3 || c.Total != 0 || c.Unresolved != 3 {
		t.Errorf("time stop = %+v", c)
	}
	if b := Actuals(ds[:3]); b.N != 3 || b.Avg != 0 || b.WinRate != 0.667 {
		t.Errorf("baseline = %+v", b)
	}
	if s := Spread([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.5, 0.9, 3, 0.25); len(s) != 3 || s[0] != 6 || s[2] != 9 {
		t.Errorf("spread = %v", s)
	}
}
