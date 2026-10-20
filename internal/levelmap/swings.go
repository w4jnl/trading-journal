package levelmap

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// ohlc is what the swing finder reads, whatever the bar type.
type ohlc struct {
	high, low float64
	at        string
}

// findSwings marks a bar whose high stands above (low below) swingWing bars
// on each side; the newest swingsPerKind of each kind are kept. The newest
// bars cannot qualify until the wing after them exists.
func findSwings(bars []ohlc, tf string, kind func(high bool, b ohlc) Level) []Level {
	var highs, lows []Level
	for i := swingWing; i < len(bars)-swingWing; i++ {
		b := bars[i]
		isHigh, isLow := true, true
		for k := 1; k <= swingWing; k++ {
			if bars[i-k].high >= b.high || bars[i+k].high >= b.high {
				isHigh = false
			}
			if bars[i-k].low <= b.low || bars[i+k].low <= b.low {
				isLow = false
			}
		}
		if isHigh {
			highs = append(highs, kind(true, b))
		}
		if isLow {
			lows = append(lows, kind(false, b))
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

func swingLevel(tf string) func(high bool, b ohlc) Level {
	return func(high bool, b ohlc) Level {
		if high {
			return Level{Kind: "swing", Name: tf + " swing high " + b.at, Price: b.high, TF: tf, Source: SourceBars, At: b.at}
		}
		return Level{Kind: "swing", Name: tf + " swing low " + b.at, Price: b.low, TF: tf, Source: SourceBars, At: b.at}
	}
}

func dailySwings(days []DayBar) []Level {
	bars := make([]ohlc, len(days))
	for i, d := range days {
		bars[i] = ohlc{d.High, d.Low, shortDay(d.Day)}
	}
	return findSwings(bars, "1d", swingLevel("1d"))
}

func weeklySwings(days []DayBar) []Level {
	var weeks []ohlc
	last := ""
	for _, d := range days {
		t, err := time.Parse("2006-01-02", d.Day)
		if err != nil {
			continue
		}
		y, w := t.ISOWeek()
		k := fmt.Sprintf("%d-W%02d", y, w)
		if k != last {
			weeks = append(weeks, ohlc{d.High, d.Low, "week " + k[len(k)-3:]})
			last = k
			continue
		}
		b := &weeks[len(weeks)-1]
		b.high, b.low = math.Max(b.high, d.High), math.Min(b.low, d.Low)
	}
	return findSwings(weeks, "1w", swingLevel("1w"))
}

func barSwings(bars []marketdata.Bar, tf string, loc *time.Location) []Level {
	o := make([]ohlc, len(bars))
	for i, b := range bars {
		o[i] = ohlc{b.High, b.Low, time.Unix(b.Time, 0).In(loc).Format("2 Jan 15:04")}
	}
	return findSwings(o, tf, swingLevel(tf))
}

// aggregateHours folds hourly bars into n-hour bars by the clock.
func aggregateHours(bars []marketdata.Bar, n int) []marketdata.Bar {
	var out []marketdata.Bar
	step := int64(n) * 3600
	for _, b := range bars {
		start := b.Time - b.Time%step
		if len(out) > 0 && out[len(out)-1].Time == start {
			o := &out[len(out)-1]
			o.High, o.Low, o.Close = math.Max(o.High, b.High), math.Min(o.Low, b.Low), b.Close
			o.Volume += b.Volume
			continue
		}
		c := b
		c.Time = start
		out = append(out, c)
	}
	return out
}

// countTouches counts, per level, the recent bars whose high or low came
// within tol of it: how often the market has leaned on the price lately.
func countTouches(levels []Level, bars []marketdata.Bar, tol float64) {
	if len(bars) == 0 {
		return
	}
	for i := range levels {
		p := levels[i].Price
		n := 0
		for _, b := range bars {
			if math.Abs(b.High-p) <= tol || math.Abs(b.Low-p) <= tol {
				n++
			}
		}
		levels[i].Touches = n
	}
}

// Zones folds levels closer than tol into bands no wider than three
// tolerances (a chain of near levels is cut rather than growing into a
// band that says nothing), strongest first.
func Zones(levels []Level, tol float64) []Zone {
	if len(levels) == 0 {
		return nil
	}
	sorted := append([]Level(nil), levels...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Price < sorted[j].Price })
	var zones []Zone
	var cur *Zone
	for _, l := range sorted {
		if cur == nil || l.Price-cur.High > tol || l.Price-cur.Low > 3*tol {
			zones = append(zones, Zone{Low: l.Price, High: l.Price})
			cur = &zones[len(zones)-1]
		}
		cur.High = l.Price
		cur.Levels = append(cur.Levels, l)
	}
	for i := range zones {
		z := &zones[i]
		sum, wsum := 0.0, 0.0
		seen := map[string]bool{}
		for _, l := range z.Levels {
			z.Score += l.Score
			sum += l.Price * l.Score
			wsum += l.Score
			if !seen[l.Kind] {
				seen[l.Kind] = true
				z.Kinds = append(z.Kinds, l.Kind)
			}
		}
		if wsum > 0 {
			z.Center = sum / wsum
		} else {
			z.Center = (z.Low + z.High) / 2
		}
		z.Score = math.Round(z.Score*100) / 100
	}
	sort.SliceStable(zones, func(i, j int) bool { return zones[i].Score > zones[j].Score })
	return zones
}
