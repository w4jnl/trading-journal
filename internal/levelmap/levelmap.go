// Package levelmap builds the day map: one set of reference levels per
// market and trading day, from the journal's own bars (daily history and
// the recent one-minute bars), the session map a MotiveWave chart posts
// (the feed, which wins where both know a level) and the gamma engine's
// walls. Everything here is arithmetic and deterministic, so the page, the
// plan draft, the review and the assistants share one definition; the
// scores rank, they do not forecast.
package levelmap

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// Sources a level can come from.
const (
	SourceBars  = "bars"
	SourceChart = "chart"
	SourceGex   = "gex"
)

// Level is one reference price.
type Level struct {
	Kind    string  `json:"kind"`   // pdh, pdl, pdc, onh, onl, orh, orl, poc, vah, val, vwap, vwap_band, atr, pivot, camarilla, period, swing, gap, round, gamma
	Name    string  `json:"name"`   // "prior day high", "ATR +0.5 (RTH)", "1h swing low 14:30"
	Price   float64 `json:"price"`  // in points of the market's full-size contract
	TF      string  `json:"tf"`     // the timeframe it was read from: 1m, 30m, 1h, 4h, 1d, 1w, 1mo, "" for a grid or a feed value
	Source  string  `json:"source"` // bars | chart | gex
	Weight  float64 `json:"weight"` // the kind's weight times the timeframe's
	Touches int     `json:"touches"`
	Score   float64 `json:"score"`
	At      string  `json:"at,omitempty"` // when it was made: a swing's time, a gap's day
}

// Zone is levels close enough to read as one: a band with a score.
type Zone struct {
	Low    float64  `json:"low"`
	High   float64  `json:"high"`
	Center float64  `json:"center"`
	Score  float64  `json:"score"`
	Kinds  []string `json:"kinds"`
	Levels []Level  `json:"levels"`
}

// Expected is the day's range from the ATR around the active anchor.
type Expected struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// Sources says what the map was built from.
type Sources struct {
	Bars     bool       `json:"bars"`
	BarsTF   string     `json:"barsTf,omitempty"` // the intraday bars' timeframe
	DailyN   int        `json:"dailyBars"`
	Chart    bool       `json:"chart"`
	ChartAt  *time.Time `json:"chartAt,omitempty"`
	Contract string     `json:"contract,omitempty"`
	Gex      bool       `json:"gex"`
	GexAt    *time.Time `json:"gexAt,omitempty"`
}

// Map is the day map of one market.
type Map struct {
	Market    string    `json:"market"`
	Day       string    `json:"day"`
	AsOf      time.Time `json:"asOf"`
	Last      float64   `json:"last"`
	LastAt    time.Time `json:"lastAt"`
	ATR       float64   `json:"atr"`       // daily ATR(14), 0 when unknown
	ATRMethod string    `json:"atrMethod"` // smma | sma | ""
	AnchorETH *float64  `json:"anchorEth,omitempty"`
	AnchorRTH *float64  `json:"anchorRth,omitempty"`
	Anchor    string    `json:"anchor"` // eth | rth | "": which anchor the bands use
	Expected  *Expected `json:"expected,omitempty"`
	Levels    []Level   `json:"levels"`
	Zones     []Zone    `json:"zones"`
	DayType   DayType   `json:"dayType"`
	Sources   Sources   `json:"sources"`
	Feed      *Feed     `json:"feed,omitempty"`
}

// Input is everything Build reads.
type Input struct {
	Market  string
	Day     string // the trading day, YYYY-MM-DD
	AsOf    time.Time
	Chicago *time.Location
	Tick    float64
	// Daily are completed daily bars ascending by day (the current day may
	// be present and is left out of the history), Intraday the recent fine
	// bars ascending (one minute, or five), IntradayTF their timeframe.
	Daily      []DayBar
	Intraday   []marketdata.Bar
	IntradayTF string
	Feed       *Feed
	Gamma      []Level // source gex, already in futures points
	GammaAt    time.Time
}

// DayBar is one trading day's bar.
type DayBar struct {
	Day    string  `json:"day"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
}

const (
	atrPeriod     = 14
	swingWing     = 2
	swingsPerKind = 3
	gapDays       = 60
	touchDays     = 10
)

var atrMults = []float64{0.25, 0.5, 0.75, 1.0}

// Build makes the map. It never fails: with no bars it returns the feed's
// levels alone, with nothing at all an empty map.
func Build(in Input) Map {
	m := Map{Market: in.Market, Day: in.Day, AsOf: in.AsOf, Sources: Sources{BarsTF: in.IntradayTF}}
	chicago := in.Chicago
	if chicago == nil {
		chicago = time.UTC
	}
	tick := in.Tick
	if tick <= 0 {
		tick = DefaultTick(in.Market)
	}
	prior := priorDays(in.Daily, in.Day)
	m.Sources.DailyN = len(prior)
	m.Sources.Bars = len(prior) > 0 || len(in.Intraday) > 0

	// The last price: the chart's mark, else the newest bar.
	var sc *marketdata.SessionContext
	if len(in.Intraday) > 0 {
		c := marketdata.ComputeSession(in.Intraday, in.AsOf, chicago)
		sc = &c
		m.Last, m.LastAt = c.Last, c.LastAt
	}
	if in.Feed != nil {
		m.Feed = in.Feed
		m.Sources.Chart, m.Sources.Contract = true, in.Feed.Contract
		at := in.Feed.AsOf
		m.Sources.ChartAt = &at
		if in.Feed.Mark > 0 {
			m.Last, m.LastAt = in.Feed.Mark, in.Feed.AsOf
		}
	}

	// The daily ATR: the chart's when it says, else Wilder's over the
	// completed days.
	if in.Feed != nil && in.Feed.ATR != nil && in.Feed.ATR.Daily14 > 0 {
		m.ATR, m.ATRMethod = in.Feed.ATR.Daily14, in.Feed.ATR.Method
		if m.ATRMethod == "" {
			m.ATRMethod = "smma"
		}
	} else if atr, ok := WilderATR(prior, atrPeriod); ok {
		m.ATR, m.ATRMethod = atr, "smma"
	}

	var levels []Level
	add := func(l Level) {
		if l.Price > 0 && !math.IsNaN(l.Price) && !math.IsInf(l.Price, 0) {
			if l.Weight == 0 {
				l.Weight = kindWeight(l.Kind) * tfWeight(l.TF)
			}
			levels = append(levels, l)
		}
	}

	// The session structure from the bars, then the chart's values over it.
	if sc != nil {
		if r := sc.PriorRTH; r != nil {
			add(Level{Kind: "pdh", Name: "prior day high", Price: r.High, TF: "1d", Source: SourceBars})
			add(Level{Kind: "pdl", Name: "prior day low", Price: r.Low, TF: "1d", Source: SourceBars})
			if r.Close != nil {
				add(Level{Kind: "pdc", Name: "prior day close", Price: *r.Close, TF: "1d", Source: SourceBars})
			}
		} else if n := len(prior); n > 0 {
			p := prior[n-1]
			add(Level{Kind: "pdh", Name: "prior day high", Price: p.High, TF: "1d", Source: SourceBars})
			add(Level{Kind: "pdl", Name: "prior day low", Price: p.Low, TF: "1d", Source: SourceBars})
			add(Level{Kind: "pdc", Name: "prior day close", Price: p.Close, TF: "1d", Source: SourceBars})
		}
		if r := sc.Overnight; r != nil {
			add(Level{Kind: "onh", Name: "overnight high", Price: r.High, TF: "1m", Source: SourceBars})
			add(Level{Kind: "onl", Name: "overnight low", Price: r.Low, TF: "1m", Source: SourceBars})
		}
		if r := sc.OpeningRange; r != nil {
			add(Level{Kind: "orh", Name: "opening range high", Price: r.High, TF: "1m", Source: SourceBars})
			add(Level{Kind: "orl", Name: "opening range low", Price: r.Low, TF: "1m", Source: SourceBars})
		}
		if sc.VWAP != nil && sc.RTH != nil {
			add(Level{Kind: "vwap", Name: "session VWAP", Price: *sc.VWAP, TF: "1m", Source: SourceBars})
		}
		if sc.RTH != nil && sc.RTH.Open != nil {
			o := *sc.RTH.Open
			m.AnchorRTH = &o
		}
		if o, ok := sessionOpen(in.Intraday, in.Day, chicago); ok {
			m.AnchorETH = &o
		}
	} else if n := len(prior); n > 0 {
		p := prior[n-1]
		add(Level{Kind: "pdh", Name: "prior day high", Price: p.High, TF: "1d", Source: SourceBars})
		add(Level{Kind: "pdl", Name: "prior day low", Price: p.Low, TF: "1d", Source: SourceBars})
		add(Level{Kind: "pdc", Name: "prior day close", Price: p.Close, TF: "1d", Source: SourceBars})
	}
	if f := in.Feed; f != nil {
		kept, added := overrideFromFeed(levels, f)
		levels = kept
		for _, l := range added {
			add(l)
		}
		if f.ATR != nil && f.ATR.Anchor > 0 {
			a := f.ATR.Anchor
			switch f.ATR.AnchoredAt {
			case "rth":
				m.AnchorRTH = &a
			case "eth":
				m.AnchorETH = &a
			}
		}
	}

	// The ATR bands from the active anchor and the expected range.
	if m.ATR > 0 {
		anchor, which := m.AnchorETH, "eth"
		if m.AnchorRTH != nil {
			anchor, which = m.AnchorRTH, "rth"
		}
		if anchor != nil {
			m.Anchor = which
			tag := strings.ToUpper(which)
			mults := atrMults
			if in.Feed != nil && in.Feed.ATR != nil && len(in.Feed.ATR.Mults) > 0 {
				mults = in.Feed.ATR.Mults
			}
			for _, k := range mults {
				if k <= 0 {
					continue
				}
				add(Level{Kind: "atr", Name: fmtMult("ATR +", k) + " (" + tag + ")", Price: snap(*anchor+k*m.ATR, tick), TF: "1d", Source: SourceBars, Weight: kindWeight("atr") * multWeight(k)})
				add(Level{Kind: "atr", Name: fmtMult("ATR -", k) + " (" + tag + ")", Price: snap(*anchor-k*m.ATR, tick), TF: "1d", Source: SourceBars, Weight: kindWeight("atr") * multWeight(k)})
			}
			m.Expected = &Expected{Low: snap(*anchor-m.ATR, tick), High: snap(*anchor+m.ATR, tick)}
		}
	}

	// Pivots, Camarilla, the prior week and month, swings, gaps, round numbers.
	if n := len(prior); n > 0 {
		p := prior[n-1]
		for _, l := range FloorPivots(p, "1d") {
			add(l)
		}
		for _, l := range Camarilla(p) {
			add(l)
		}
		if w, ok := priorPeriod(prior, in.Day, "week"); ok {
			add(Level{Kind: "period", Name: "prior week high", Price: w.High, TF: "1w", Source: SourceBars})
			add(Level{Kind: "period", Name: "prior week low", Price: w.Low, TF: "1w", Source: SourceBars})
			for _, l := range FloorPivots(w, "1w") {
				add(l)
			}
		}
		if mo, ok := priorPeriod(prior, in.Day, "month"); ok {
			add(Level{Kind: "period", Name: "prior month high", Price: mo.High, TF: "1mo", Source: SourceBars})
			add(Level{Kind: "period", Name: "prior month low", Price: mo.Low, TF: "1mo", Source: SourceBars})
		}
		for _, l := range dailySwings(prior) {
			add(l)
		}
		for _, l := range weeklySwings(prior) {
			add(l)
		}
		for _, l := range Gaps(prior, tick, m.ATR) {
			add(l)
		}
	}
	for _, tf := range []marketdata.Timeframe{marketdata.TF30m, marketdata.TF1h} {
		if len(in.Intraday) == 0 || marketdata.Timeframe(in.IntradayTF).Duration() > tf.Duration() {
			continue
		}
		agg := marketdata.AggregateTo(in.Intraday, tf)
		for _, l := range barSwings(agg, string(tf), chicago) {
			add(l)
		}
	}
	if len(in.Intraday) > 0 && marketdata.Timeframe(in.IntradayTF).Duration() <= time.Hour {
		agg := aggregateHours(marketdata.AggregateTo(in.Intraday, marketdata.TF1h), 4)
		for _, l := range barSwings(agg, "4h", chicago) {
			add(l)
		}
	}
	if m.Last > 0 {
		span := 1.5 * m.ATR
		if span <= 0 {
			span = m.Last * 0.01
		}
		for _, l := range RoundNumbers(in.Market, m.Last, span) {
			add(l)
		}
	}
	for _, g := range in.Gamma {
		g.Source = SourceGex
		if g.Kind == "" {
			g.Kind = "gamma"
		}
		add(g)
	}
	if len(in.Gamma) > 0 {
		m.Sources.Gex = true
		at := in.GammaAt
		m.Sources.GexAt = &at
	}

	// Touches, scores, zones.
	tol := tolerance(m.ATR, tick)
	if len(in.Intraday) > 0 {
		countTouches(levels, marketdata.AggregateTo(in.Intraday, marketdata.TF30m), tol)
	}
	for i := range levels {
		levels[i].Score = levels[i].Weight * (1 + 0.15*math.Min(float64(levels[i].Touches), 5))
	}
	sort.SliceStable(levels, func(i, j int) bool { return levels[i].Price > levels[j].Price })
	m.Levels = levels
	m.Zones = Zones(levels, tol)
	m.DayType = ClassifyDay(prior, sc, m.ATR, m.Last, m.AnchorRTH, tick)
	return m
}

// priorDays are the completed days before day, ascending.
func priorDays(days []DayBar, day string) []DayBar {
	var out []DayBar
	for _, d := range days {
		if d.Day < day && d.High > 0 {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// sessionOpen is the first bar's open of the trading day: the ETH anchor.
func sessionOpen(bars []marketdata.Bar, day string, chicago *time.Location) (float64, bool) {
	for _, b := range bars {
		if TradingDay(time.Unix(b.Time, 0), chicago) == day {
			return b.Open, true
		}
	}
	return 0, false
}

// TradingDay folds a time into its CME trading day: 17:00 Chicago rolls to
// the next calendar day.
func TradingDay(t time.Time, chicago *time.Location) string {
	lt := t.In(chicago)
	if lt.Hour()*60+lt.Minute() >= 17*60 {
		lt = lt.AddDate(0, 0, 1)
	}
	return lt.Format("2006-01-02")
}

// TradingDays folds fine bars into trading-day bars, ascending.
func TradingDays(bars []marketdata.Bar, chicago *time.Location) []DayBar {
	var out []DayBar
	idx := map[string]int{}
	for _, b := range bars {
		day := TradingDay(time.Unix(b.Time, 0), chicago)
		i, ok := idx[day]
		if !ok {
			idx[day] = len(out)
			out = append(out, DayBar{Day: day, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
			continue
		}
		d := &out[i]
		d.High, d.Low, d.Close = math.Max(d.High, b.High), math.Min(d.Low, b.Low), b.Close
		d.Volume += b.Volume
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// DaysFromDaily turns vendor daily bars (one per calendar day, bar time at
// the day's start in the vendor's zone) into day bars keyed by the UTC date.
func DaysFromDaily(bars []marketdata.Bar) []DayBar {
	out := make([]DayBar, 0, len(bars))
	for _, b := range bars {
		out = append(out, DayBar{Day: time.Unix(b.Time, 0).UTC().Format("2006-01-02"), Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
	}
	return out
}

// MergeDays prefers the second series where both have a day (the exact
// contract's trading days over the vendor's continuous series).
func MergeDays(base, over []DayBar) []DayBar {
	byDay := map[string]DayBar{}
	for _, d := range base {
		byDay[d.Day] = d
	}
	for _, d := range over {
		byDay[d.Day] = d
	}
	out := make([]DayBar, 0, len(byDay))
	for _, d := range byDay {
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// fmtMult writes "ATR +0.5": the multiple without trailing zeros.
func fmtMult(prefix string, k float64) string {
	return prefix + strconv.FormatFloat(k, 'f', -1, 64)
}

func snap(price, tick float64) float64 {
	if tick <= 0 {
		return math.Round(price*100) / 100
	}
	return math.Round(price/tick) * tick
}

// tolerance is how close two levels must be to read as one zone: a tenth
// of the ATR, never under two ticks.
func tolerance(atr, tick float64) float64 {
	t := atr * 0.1
	if t < 2*tick {
		t = 2 * tick
	}
	if t <= 0 {
		t = 1
	}
	return t
}
