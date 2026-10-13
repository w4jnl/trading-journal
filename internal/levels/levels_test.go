package levels

import (
	"strings"
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

func find(cs []Candidate, kind, source string) []Candidate {
	var out []Candidate
	for _, c := range cs {
		if c.Kind == kind && c.Source == source {
			out = append(out, c)
		}
	}
	return out
}

func TestCandidatesLong(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 13, 50, 0, 0, time.UTC)
	// A dip to 6995 at 13:52 (swing low), a push to 7008 at 13:56 (swing
	// high), then back to the entry area.
	before := bars(t0,
		[2]float64{6999, 7001}, [2]float64{6997, 6999}, [2]float64{6995, 6997}, [2]float64{6997, 7000},
		[2]float64{7000, 7004}, [2]float64{7003, 7006}, [2]float64{7005, 7008}, [2]float64{7003, 7006},
		[2]float64{7000, 7003}, [2]float64{6999, 7001})
	heat, best, best8 := 3.0, 6.0, 12.0
	in := Input{Direction: "long", Entry: 7000, EntryTime: t0.Add(10 * time.Minute).Unix(), Tick: 0.25, Before: before,
		History: &History{Scope: "strategy ORB", N: 20, WinnerHeat: &heat, WinnerBest: &best, WinnerBest8: &best8},
		Gamma:   []GammaLevel{{"call wall", 7020}, {"gamma flip", 6990}, {"put wall", 6800}}}
	cs := Candidates(in)

	st := find(cs, "stop", "structure")
	if len(st) == 0 || st[0].Price != 6994.75 || !strings.Contains(st[0].Note, "swing low 13:52") {
		t.Errorf("structure stops = %+v", st)
	}
	tg := find(cs, "target", "structure")
	if len(tg) == 0 || tg[0].Price != 7008 || !strings.Contains(tg[0].Note, "swing high 13:56") {
		t.Errorf("structure targets = %+v", tg)
	}
	if h := find(cs, "stop", "history"); len(h) != 1 || h[0].Price != 6996.75 || h[0].Points != 3.25 {
		t.Errorf("history stop = %+v", h)
	}
	if h := find(cs, "target", "history"); len(h) != 2 || h[0].Price != 7006 || h[1].Price != 7012 {
		t.Errorf("history targets = %+v", h)
	}
	g := find(cs, "target", "gamma")
	if len(g) != 1 || g[0].Price != 7020 || g[0].Note != "call wall" {
		t.Errorf("gamma target = %+v", g)
	}
	if g := find(cs, "stop", "gamma"); len(g) != 1 || g[0].Price != 6989.75 {
		t.Errorf("gamma stop (beyond the flip, the far put wall dropped) = %+v", g)
	}
	if v := find(cs, "stop", "volatility"); len(v) != 1 || v[0].Points <= 0 {
		t.Errorf("volatility stop = %+v", v)
	}
	// Every stop below, every target above; stops listed first, nearest first.
	seenTarget := false
	for i, c := range cs {
		if c.Kind == "stop" && (c.Price >= 7000 || seenTarget) || c.Kind == "target" && c.Price <= 7000 {
			t.Errorf("candidate %d on the wrong side or out of order: %+v", i, c)
		}
		seenTarget = seenTarget || c.Kind == "target"
	}
}

func TestCandidatesShortMirror(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 13, 50, 0, 0, time.UTC)
	before := bars(t0, [2]float64{7001, 7003}, [2]float64{7003, 7005}, [2]float64{7005, 7009}, [2]float64{7003, 7005}, [2]float64{7000, 7002})
	cs := Candidates(Input{Direction: "short", Entry: 7000, EntryTime: t0.Add(5 * time.Minute).Unix(), Tick: 0.25, Before: before})
	st := find(cs, "stop", "structure")
	if len(st) == 0 || st[0].Price != 7009.25 {
		t.Errorf("short structure stop = %+v", st)
	}
	for _, c := range cs {
		if c.Kind == "stop" && c.Price <= 7000 || c.Kind == "target" && c.Price >= 7000 {
			t.Errorf("short candidate on the wrong side: %+v", c)
		}
	}
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

// Only the three nearest swing levels per side survive.
func TestCandidatesKeepNearestSwings(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
	var lh [][2]float64
	for i := 0; i < 40; i++ { // a zigzag: a swing low every 4 bars, lower each time
		base := 7000 - float64(i/4)
		lows := [4]float64{base, base - 1, base, base + 0.5}
		lh = append(lh, [2]float64{lows[i%4], lows[i%4] + 1})
	}
	cs := Candidates(Input{Direction: "long", Entry: 7001, EntryTime: t0.Add(40 * time.Minute).Unix(), Tick: 0.25, Before: bars(t0, lh...)})
	n := 0
	for _, c := range cs {
		if c.Kind == "stop" && strings.Contains(c.Note, "swing low") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("swing-low stops kept = %d, want 3", n)
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
