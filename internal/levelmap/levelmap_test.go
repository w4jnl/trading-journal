package levelmap

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

var chicago = func() *time.Location { l, _ := time.LoadLocation("America/Chicago"); return l }()

// days makes n completed daily bars ending the day before `end`, a gentle
// uptrend with a 40-point range.
func days(n int, end string) []DayBar {
	t, _ := time.Parse("2006-01-02", end)
	var out []DayBar
	for i := n; i >= 1; i-- {
		d := t.AddDate(0, 0, -i)
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		base := 6700 + float64(n-i)*5
		out = append(out, DayBar{Day: d.Format("2006-01-02"), Open: base + 5, High: base + 40, Low: base, Close: base + 30, Volume: 1000})
	}
	return out
}

// minutes makes one-minute bars for a trading day: Globex from 17:00 the
// evening before through asOf, drifting between lo and hi.
func minutes(day string, asOf time.Time, lo, hi float64) []marketdata.Bar {
	d, _ := time.ParseInLocation("2006-01-02", day, chicago)
	start := d.AddDate(0, 0, -1).Add(17 * time.Hour)
	var out []marketdata.Bar
	i := 0
	for t := start; t.Before(asOf); t = t.Add(time.Minute) {
		if t.Weekday() == time.Saturday {
			continue
		}
		phase := math.Sin(float64(i) / 90)
		p := lo + (hi-lo)*(0.5+0.45*phase)
		out = append(out, marketdata.Bar{Time: t.Unix(), Open: p, High: p + 1.25, Low: p - 1.25, Close: p + 0.25, Volume: 50})
		i++
	}
	return out
}

func TestWilderATRAndPivots(t *testing.T) {
	ds := days(30, "2026-10-20")
	atr, ok := WilderATR(ds, 14)
	if !ok || atr < 40 || atr > 46 {
		t.Fatalf("ATR = %v ok=%v, want about 40-46 (40-point ranges plus the gaps)", atr, ok)
	}
	if _, ok := WilderATR(ds[:10], 14); ok {
		t.Error("ten days must not make a 14-day ATR")
	}
	p := DayBar{High: 6800, Low: 6760, Close: 6790}
	piv := FloorPivots(p, "1d")
	want := map[string]float64{"pivot": 6783.3333, "R1": 6806.6667, "S1": 6766.6667, "R2": 6823.3333, "S2": 6743.3333}
	for _, l := range piv {
		if w, ok := want[l.Name]; ok && math.Abs(l.Price-w) > 0.01 {
			t.Errorf("%s = %.4f, want %.4f", l.Name, l.Price, w)
		}
	}
	cam := Camarilla(p)
	if cam[0].Name != "Camarilla H4" || math.Abs(cam[0].Price-6812) > 0.01 || math.Abs(cam[3].Price-6768) > 0.01 {
		t.Errorf("camarilla = %+v", cam)
	}
	w, ok := priorPeriod(ds, "2026-10-20", "week")
	if !ok || w.Day != "2026-W42" {
		t.Errorf("prior week = %+v ok=%v, want the week of 12 Oct", w, ok)
	}
	if mo, ok := priorPeriod(ds, "2026-10-20", "month"); !ok || mo.Day != "2026-09" {
		t.Errorf("prior month = %+v ok=%v", mo, ok)
	}
}

func TestGapsSwingsZones(t *testing.T) {
	// Three days: the second gaps down 20 from the first close and never
	// trades back to it; the third gaps up 10 and fills it the same day.
	ds := []DayBar{
		{Day: "2026-10-14", Open: 6800, High: 6820, Low: 6790, Close: 6810},
		{Day: "2026-10-15", Open: 6790, High: 6795, Low: 6770, Close: 6780},
		{Day: "2026-10-16", Open: 6790, High: 6800, Low: 6775, Close: 6795},
	}
	gaps := Gaps(ds, 0.25, 40)
	if len(gaps) != 1 || gaps[0].Price != 6810 || !strings.Contains(gaps[0].Name, "down 20") || !strings.Contains(gaps[0].Name, "0.5 ATR") || gaps[0].At != "2026-10-15" {
		t.Errorf("gaps = %+v, want the one unfilled down gap at 6810", gaps)
	}
	bars := []ohlc{{10, 5, "a"}, {12, 6, "b"}, {20, 9, "c"}, {13, 7, "d"}, {11, 2, "e"}, {12, 4, "f"}, {13, 6, "g"}}
	sw := findSwings(bars, "1h", swingLevel("1h"))
	if len(sw) != 2 || sw[0].Price != 20 || sw[1].Price != 2 {
		t.Errorf("swings = %+v, want the high at c and the low at e", sw)
	}
	levels := []Level{{Kind: "pdh", Price: 6800, Score: 3}, {Kind: "pivot", Price: 6800.5, Score: 1.5}, {Kind: "pdl", Price: 6760, Score: 3}}
	z := Zones(levels, 1)
	if len(z) != 2 || z[0].High != 6800.5 || len(z[0].Levels) != 2 || z[0].Score != 4.5 || z[1].Low != 6760 {
		t.Errorf("zones = %+v", z)
	}
}

func TestBuildFromBarsThenFeed(t *testing.T) {
	day := "2026-10-20"
	asOf := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC) // 09:00 Chicago, RTH open passed
	ds := days(30, day)
	last := ds[len(ds)-1]
	bars := minutes(day, asOf, last.Close-30, last.Close+20)
	in := Input{Market: "ES", Day: day, AsOf: asOf, Chicago: chicago, Tick: 0.25, Daily: ds, Intraday: bars, IntradayTF: "1m"}
	m := Build(in)
	if m.ATR <= 0 || m.ATRMethod != "smma" || m.Last <= 0 || !m.Sources.Bars || m.Sources.DailyN != len(ds) {
		t.Fatalf("map head = atr %v %s last %v sources %+v", m.ATR, m.ATRMethod, m.Last, m.Sources)
	}
	kinds := map[string]int{}
	for _, l := range m.Levels {
		kinds[l.Kind]++
		if l.Source != SourceBars {
			t.Errorf("level %s from %s without a feed", l.Name, l.Source)
		}
	}
	for _, k := range []string{"pdh", "pdl", "pdc", "onh", "onl", "orh", "orl", "atr", "pivot", "camarilla", "period", "swing", "round"} {
		if kinds[k] == 0 {
			t.Errorf("no %s level: %v", k, kinds)
		}
	}
	if m.AnchorETH == nil || m.AnchorRTH == nil || m.Anchor != "rth" || m.Expected == nil {
		t.Errorf("anchors eth=%v rth=%v which=%q expected=%v", m.AnchorETH, m.AnchorRTH, m.Anchor, m.Expected)
	}
	if len(m.Zones) == 0 || m.Zones[0].Score < m.Zones[len(m.Zones)-1].Score {
		t.Errorf("zones not ranked: %d", len(m.Zones))
	}
	if _, ok := m.DayType.Trend["1d"]; m.DayType.PriorDay != last.Day || m.DayType.Close != "strong" || m.DayType.Sentence == "" || !ok {
		t.Errorf("day type = %+v", m.DayType)
	}
	for i := 1; i < len(m.Levels); i++ {
		if m.Levels[i].Price > m.Levels[i-1].Price {
			t.Fatalf("levels not descending at %d", i)
		}
	}
	// The chart's feed replaces the bar-derived session levels and adds the profile.
	pdh, poc := 6900.0, 6880.0
	in.Feed = &Feed{Contract: "ESZ6", AsOf: asOf, Mark: 6890, Session: &FeedSess{Pdh: &pdh, Poc: &poc, ProfileApprox: true, OR: &FeedOR{Minutes: 15, High: 6895, Low: 6885, Final: true}},
		VWAP: &FeedVWAP{Value: 6888, SD1: 3}, ATR: &FeedATR{Daily14: 50, Method: "sma", Anchor: 6887, AnchoredAt: "rth", Mults: []float64{0.5, 1}}}
	m2 := Build(in)
	if !m2.Sources.Chart || m2.Last != 6890 || m2.ATR != 50 || m2.ATRMethod != "sma" || *m2.AnchorRTH != 6887 {
		t.Errorf("feed not applied: %+v last %v atr %v", m2.Sources, m2.Last, m2.ATR)
	}
	got := map[string]Level{}
	for _, l := range m2.Levels {
		if l.Kind == "pdh" || l.Kind == "poc" || l.Kind == "orh" || l.Kind == "vwap" || l.Kind == "pdl" {
			if _, dup := got[l.Kind]; dup {
				t.Errorf("two %s levels after the feed", l.Kind)
			}
			got[l.Kind] = l
		}
	}
	if got["pdh"].Price != 6900 || got["pdh"].Source != SourceChart || got["poc"].Price != 6880 || !strings.Contains(got["poc"].Name, "approx") ||
		got["orh"].Price != 6895 || got["vwap"].Source != SourceChart || got["pdl"].Source != SourceBars {
		t.Errorf("feed levels = %+v", got)
	}
	atrs := 0
	for _, l := range m2.Levels {
		if l.Kind == "atr" {
			atrs++
		}
	}
	if atrs != 4 {
		t.Errorf("ATR bands = %d, want 4 from the feed's two multiples", atrs)
	}
	if b, err := json.Marshal(m2); err != nil || len(b) > 64<<10 {
		t.Errorf("json = %d bytes, %v", len(b), err)
	}
}

func TestFeedValidate(t *testing.T) {
	now := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	ok := &Feed{Contract: "ESZ6", AsOf: now.Add(-time.Minute), Mark: 6800}
	if err := ok.Validate(now); err != nil {
		t.Errorf("minimal feed refused: %v", err)
	}
	neg := -1.0
	for name, f := range map[string]*Feed{
		"no contract": {AsOf: now, Mark: 1},
		"no asOf":     {Contract: "ESZ6", Mark: 1},
		"future":      {Contract: "ESZ6", AsOf: now.Add(time.Hour), Mark: 1},
		"old":         {Contract: "ESZ6", AsOf: now.Add(-4 * 24 * time.Hour), Mark: 1},
		"no mark":     {Contract: "ESZ6", AsOf: now},
		"bad price":   {Contract: "ESZ6", AsOf: now, Mark: 1, Session: &FeedSess{Pdh: &neg}},
		"bad or":      {Contract: "ESZ6", AsOf: now, Mark: 1, Session: &FeedSess{OR: &FeedOR{Minutes: 7, High: 2, Low: 1}}},
		"bad anchor":  {Contract: "ESZ6", AsOf: now, Mark: 1, ATR: &FeedATR{Daily14: 10, AnchoredAt: "noon"}},
	} {
		if err := f.Validate(now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestTradingDays(t *testing.T) {
	asOf := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	bars := minutes("2026-10-20", asOf, 6700, 6750)
	ds := TradingDays(bars, chicago)
	if len(ds) != 1 || ds[0].Day != "2026-10-20" || ds[0].Open != bars[0].Open {
		t.Errorf("trading days = %+v", ds)
	}
	if TradingDay(time.Date(2026, 10, 19, 22, 30, 0, 0, time.UTC), chicago) != "2026-10-20" {
		t.Error("17:30 Chicago belongs to the next trading day")
	}
	if TradingDay(time.Date(2026, 10, 19, 20, 30, 0, 0, time.UTC), chicago) != "2026-10-19" {
		t.Error("15:30 Chicago belongs to the same day")
	}
	m, _ := MarketOf("MES")
	if m != "ES" || !IsMarket("NQ") || IsMarket("SPX") {
		t.Error("market mapping")
	}
}
