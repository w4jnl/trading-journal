package marketdata

import (
	"math"
	"sort"
	"time"
)

// ---- Session context ---------------------------------------------------
//
// What a trader reads off the last two sessions before placing a stop or a
// target, computed from the cached one-minute bars: the prior RTH range and
// close, the overnight range, the opening range, the session's extremes so
// far, VWAP, ATR and the last swing highs and lows on 5m and 15m. Pure and
// deterministic, so the AI prompts, the review, the guardrail and the lab
// can share one definition. CME conventions: the trading day runs from
// 17:00 to 16:00 Chicago (so Globex before the open belongs to the day it
// precedes), RTH is 08:30 to 15:00 Chicago.

const (
	rthStartMin   = 8*60 + 30
	rthEndMin     = 15 * 60
	sessionTurn   = 17 * 60 // minutes of day the trading day rolls over
	openingMins   = 15      // the opening range: first 15 minutes of RTH
	atrPeriod     = 14
	swingsPerKind = 3 // last swing highs and lows kept, per timeframe
	swingWing     = 2 // bars on each side a swing must stand above / below
)

// Range is a high and a low (and, for a completed RTH, its open and close).
type Range struct {
	High  float64  `json:"high"`
	Low   float64  `json:"low"`
	Open  *float64 `json:"open,omitempty"`
	Close *float64 `json:"close,omitempty"`
	Day   string   `json:"day,omitempty"` // the trading day it belongs to, YYYY-MM-DD
}

// Swing is a local extreme on one timeframe.
type Swing struct {
	Time  time.Time `json:"time"`
	Price float64   `json:"price"`
	Kind  string    `json:"kind"` // high | low
	TF    string    `json:"tf"`
}

// SessionContext is the structure around the last price, as of AsOf.
type SessionContext struct {
	AsOf     time.Time `json:"asOf"`
	Last     float64   `json:"last"`
	LastAt   time.Time `json:"lastAt"`
	Bars1m   int       `json:"bars1m"` // how many one-minute bars it was read from
	TradeDay string    `json:"tradeDay"`
	// The completed RTH before the current session, its overnight (Globex
	// from the roll to the RTH open) and the opening range once RTH began.
	PriorRTH     *Range `json:"priorRth,omitempty"`
	Overnight    *Range `json:"overnight,omitempty"`
	OpeningRange *Range `json:"openingRange,omitempty"`
	// The current trading day's extremes so far and its RTH so far.
	Session *Range `json:"session,omitempty"`
	RTH     *Range `json:"rth,omitempty"`
	// Volume-weighted average price of the current trading day, nil when the
	// bars carry no volume.
	VWAP *float64 `json:"vwap,omitempty"`
	// Average true range over the last 14 bars, per timeframe, in points.
	ATR    map[string]float64 `json:"atr,omitempty"`
	Swings []Swing            `json:"swings,omitempty"`
}

// tradeDayKey folds a bar time into its CME trading day (Chicago wall clock,
// 17:00 rolls to the next day).
func tradeDayKey(t time.Time, chicago *time.Location) string {
	lt := t.In(chicago)
	if lt.Hour()*60+lt.Minute() >= sessionTurn {
		lt = lt.AddDate(0, 0, 1)
	}
	return lt.Format("2006-01-02")
}

func minuteOfDay(t time.Time, chicago *time.Location) int {
	lt := t.In(chicago)
	return lt.Hour()*60 + lt.Minute()
}

func inRTH(t time.Time, chicago *time.Location) bool {
	m := minuteOfDay(t, chicago)
	return m >= rthStartMin && m < rthEndMin
}

// ComputeSession reads the context from one-minute bars (ascending, UTC
// times) as of asOf: bars at or after asOf are ignored, so a closed trade
// can be judged with what was known at its entry. An empty input yields a
// zero context with Bars1m 0.
func ComputeSession(bars []Bar, asOf time.Time, chicago *time.Location) SessionContext {
	ctx := SessionContext{AsOf: asOf, ATR: map[string]float64{}}
	var in []Bar
	for _, b := range bars {
		if time.Unix(b.Time, 0).Before(asOf) {
			in = append(in, b)
		}
	}
	if len(in) == 0 {
		return ctx
	}
	ctx.Bars1m = len(in)
	last := in[len(in)-1]
	ctx.Last, ctx.LastAt = last.Close, time.Unix(last.Time, 0).UTC()
	ctx.TradeDay = tradeDayKey(ctx.LastAt, chicago)

	type dayAcc struct {
		session, rth, overnight, opening *Range
		pv, vol                          float64
		rthOpen, rthClose                float64
		rthLast                          int64
		openingEnd                       int
	}
	days := map[string]*dayAcc{}
	var order []string
	for _, b := range in {
		t := time.Unix(b.Time, 0)
		key := tradeDayKey(t, chicago)
		d := days[key]
		if d == nil {
			d = &dayAcc{}
			days[key] = d
			order = append(order, key)
		}
		d.session = widen(d.session, b)
		d.pv += (b.High + b.Low + b.Close) / 3 * b.Volume
		d.vol += b.Volume
		if inRTH(t, chicago) {
			if d.rth == nil {
				d.rthOpen = b.Open
				d.openingEnd = minuteOfDay(t, chicago) + openingMins
			}
			d.rth = widen(d.rth, b)
			d.rthClose, d.rthLast = b.Close, b.Time
			if minuteOfDay(t, chicago) < rthStartMin+openingMins {
				d.opening = widen(d.opening, b)
			}
		} else if d.rth == nil {
			d.overnight = widen(d.overnight, b)
		}
	}
	sort.Strings(order)
	cur := days[ctx.TradeDay]
	if cur != nil {
		ctx.Session = cur.session
		ctx.Overnight = cur.overnight
		if cur.rth != nil {
			r := *cur.rth
			o := cur.rthOpen
			r.Open = &o
			ctx.RTH = &r
			// The opening range is complete once RTH is that old.
			if cur.opening != nil && minuteOfDay(ctx.LastAt, chicago) >= rthStartMin+openingMins {
				ctx.OpeningRange = cur.opening
			}
		}
		if cur.vol > 0 {
			v := cur.pv / cur.vol
			ctx.VWAP = &v
		}
	}
	// The prior RTH: the newest completed RTH of an earlier trading day.
	for i := len(order) - 1; i >= 0; i-- {
		if order[i] >= ctx.TradeDay {
			continue
		}
		d := days[order[i]]
		if d.rth == nil {
			continue
		}
		r := *d.rth
		o, c := d.rthOpen, d.rthClose
		r.Open, r.Close, r.Day = &o, &c, order[i]
		ctx.PriorRTH = &r
		break
	}
	for _, tf := range []Timeframe{TF5m, TF15m} {
		agg := AggregateTo(in, tf)
		if atr, ok := averageTrueRange(agg, atrPeriod); ok {
			ctx.ATR[string(tf)] = atr
		}
		ctx.Swings = append(ctx.Swings, swings(agg, string(tf))...)
	}
	if len(ctx.ATR) == 0 {
		ctx.ATR = nil
	}
	sort.SliceStable(ctx.Swings, func(i, j int) bool { return ctx.Swings[i].Time.Before(ctx.Swings[j].Time) })
	return ctx
}

func widen(r *Range, b Bar) *Range {
	if r == nil {
		return &Range{High: b.High, Low: b.Low}
	}
	r.High, r.Low = math.Max(r.High, b.High), math.Min(r.Low, b.Low)
	return r
}

// averageTrueRange is the simple mean of the true range over the last n
// bars; ok is false with fewer than n+1 bars.
func averageTrueRange(bars []Bar, n int) (float64, bool) {
	if len(bars) < n+1 {
		return 0, false
	}
	sum := 0.0
	for i := len(bars) - n; i < len(bars); i++ {
		b, prev := bars[i], bars[i-1]
		tr := math.Max(b.High-b.Low, math.Max(math.Abs(b.High-prev.Close), math.Abs(b.Low-prev.Close)))
		sum += tr
	}
	return sum / float64(n), true
}

// swings finds the last swing highs and lows (a bar whose high stands above,
// or low below, swingWing bars on each side), newest last, at most
// swingsPerKind of each. The newest bars cannot qualify until the wing
// after them exists, which is the honest reading as of now.
func swings(bars []Bar, tf string) []Swing {
	var highs, lows []Swing
	for i := swingWing; i < len(bars)-swingWing; i++ {
		b := bars[i]
		isHigh, isLow := true, true
		for k := 1; k <= swingWing; k++ {
			if bars[i-k].High >= b.High || bars[i+k].High >= b.High {
				isHigh = false
			}
			if bars[i-k].Low <= b.Low || bars[i+k].Low <= b.Low {
				isLow = false
			}
		}
		t := time.Unix(b.Time, 0).UTC()
		if isHigh {
			highs = append(highs, Swing{Time: t, Price: b.High, Kind: "high", TF: tf})
		}
		if isLow {
			lows = append(lows, Swing{Time: t, Price: b.Low, Kind: "low", TF: tf})
		}
	}
	if len(highs) > swingsPerKind {
		highs = highs[len(highs)-swingsPerKind:]
	}
	if len(lows) > swingsPerKind {
		lows = lows[len(lows)-swingsPerKind:]
	}
	return append(highs, lows...)
}

// Levels lists the context's structural prices with a name, nearest to the
// last price first: what a stop or a target is placed against.
func (c SessionContext) Levels() []NamedLevel {
	var out []NamedLevel
	add := func(name string, p *float64) {
		if p != nil {
			out = append(out, NamedLevel{Name: name, Price: *p})
		}
	}
	if r := c.PriorRTH; r != nil {
		add("prior RTH high", &r.High)
		add("prior RTH low", &r.Low)
		add("prior RTH close", r.Close)
	}
	if r := c.Overnight; r != nil {
		add("overnight high", &r.High)
		add("overnight low", &r.Low)
	}
	if r := c.OpeningRange; r != nil {
		add("opening range high", &r.High)
		add("opening range low", &r.Low)
	}
	if r := c.RTH; r != nil {
		add("RTH open", r.Open)
		add("session high", &r.High)
		add("session low", &r.Low)
	} else if r := c.Session; r != nil {
		add("session high", &r.High)
		add("session low", &r.Low)
	}
	add("VWAP", c.VWAP)
	for _, s := range c.Swings {
		out = append(out, NamedLevel{Name: s.TF + " swing " + s.Kind + " " + s.Time.In(time.UTC).Format("15:04") + "Z", Price: s.Price})
	}
	for i := range out {
		out[i].Distance = out[i].Price - c.Last
	}
	sort.SliceStable(out, func(i, j int) bool { return math.Abs(out[i].Distance) < math.Abs(out[j].Distance) })
	return out
}

// NamedLevel is one structural price and its distance from the last price
// (positive above it).
type NamedLevel struct {
	Name     string  `json:"name"`
	Price    float64 `json:"price"`
	Distance float64 `json:"distance"`
}
