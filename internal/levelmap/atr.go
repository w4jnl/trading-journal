package levelmap

import (
	"fmt"
	"math"
	"time"
)

// WilderATR is the smoothed (SMMA, Wilder) average true range over the
// last n completed days; ok is false with fewer than n+1 days. The first
// value is the simple mean, then each day blends in at 1/n, as the
// DailyAtrLevels study does with SMMA.
func WilderATR(days []DayBar, n int) (float64, bool) {
	if len(days) < n+1 || n < 1 {
		return 0, false
	}
	trs := make([]float64, 0, len(days)-1)
	for i := 1; i < len(days); i++ {
		trs = append(trs, trueRange(days[i], days[i-1].Close))
	}
	atr := 0.0
	for _, tr := range trs[:n] {
		atr += tr
	}
	atr /= float64(n)
	for _, tr := range trs[n:] {
		atr = (atr*float64(n-1) + tr) / float64(n)
	}
	return atr, true
}

func trueRange(d DayBar, prevClose float64) float64 {
	return math.Max(d.High-d.Low, math.Max(math.Abs(d.High-prevClose), math.Abs(d.Low-prevClose)))
}

// FloorPivots are the classic pivots from one period's high, low and close.
func FloorPivots(p DayBar, tf string) []Level {
	pp := (p.High + p.Low + p.Close) / 3
	rng := p.High - p.Low
	label := map[string]string{"1d": "", "1w": "weekly ", "1mo": "monthly "}[tf]
	mk := func(name string, price float64) Level {
		return Level{Kind: "pivot", Name: label + name, Price: price, TF: tf, Source: SourceBars}
	}
	return []Level{
		mk("pivot", pp),
		mk("R1", 2*pp-p.Low), mk("S1", 2*pp-p.High),
		mk("R2", pp+rng), mk("S2", pp-rng),
		mk("R3", p.High+2*(pp-p.Low)), mk("S3", p.Low-2*(p.High-pp)),
	}
}

// Camarilla are the H3/H4 and L3/L4 bands from the prior day.
func Camarilla(p DayBar) []Level {
	rng := p.High - p.Low
	mk := func(name string, price float64) Level {
		return Level{Kind: "camarilla", Name: "Camarilla " + name, Price: price, TF: "1d", Source: SourceBars}
	}
	return []Level{
		mk("H4", p.Close+rng*1.1/2), mk("H3", p.Close+rng*1.1/4),
		mk("L3", p.Close-rng*1.1/4), mk("L4", p.Close-rng*1.1/2),
	}
}

// priorPeriod folds the completed ISO week or calendar month before the
// day into one bar.
func priorPeriod(days []DayBar, day, period string) (DayBar, bool) {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return DayBar{}, false
	}
	key := func(d string) string {
		dt, err := time.Parse("2006-01-02", d)
		if err != nil {
			return ""
		}
		if period == "week" {
			y, w := dt.ISOWeek()
			return fmt.Sprintf("%d-W%02d", y, w)
		}
		return dt.Format("2006-01")
	}
	cur := key(t.Format("2006-01-02"))
	var out DayBar
	found := false
	last := ""
	for i := len(days) - 1; i >= 0; i-- {
		k := key(days[i].Day)
		if k == "" || k >= cur {
			continue
		}
		if !found {
			last = k
			out = days[i]
			out.Day = k
			found = true
			continue
		}
		if k != last {
			break
		}
		out.High, out.Low = math.Max(out.High, days[i].High), math.Min(out.Low, days[i].Low)
		out.Open = days[i].Open
		out.Volume += days[i].Volume
	}
	return out, found
}

// Gaps lists the unfilled gaps of the last gapDays: a day that opened away
// from the prior close by more than a tick and whose fill price no later
// day traded through. The level is the fill, the prior close.
func Gaps(days []DayBar, tick, atr float64) []Level {
	var out []Level
	start := len(days) - gapDays
	if start < 1 {
		start = 1
	}
	for i := start; i < len(days); i++ {
		prev, d := days[i-1], days[i]
		gap := d.Open - prev.Close
		if math.Abs(gap) <= tick {
			continue
		}
		filled := false
		for _, later := range days[i:] {
			if later.Low <= prev.Close && later.High >= prev.Close {
				filled = true
				break
			}
		}
		if filled {
			continue
		}
		dir := "up"
		if gap < 0 {
			dir = "down"
		}
		name := fmt.Sprintf("gap fill, %s %s on %s", dir, fmtPts(math.Abs(gap)), shortDay(d.Day))
		if atr > 0 {
			name = fmt.Sprintf("gap fill, %s %s (%.1f ATR) on %s", dir, fmtPts(math.Abs(gap)), math.Abs(gap)/atr, shortDay(d.Day))
		}
		out = append(out, Level{Kind: "gap", Name: name, Price: prev.Close, TF: "1d", Source: SourceBars, At: d.Day})
	}
	return out
}

// RoundNumbers are the market's round-number grid within span of the price:
// the minor step at a small weight, the major step at a larger one.
func RoundNumbers(market string, last, span float64) []Level {
	minor, major := roundSteps(market)
	var out []Level
	lo, hi := last-span, last+span
	for p := math.Floor(lo/minor) * minor; p <= hi; p += minor {
		if p <= 0 {
			continue
		}
		isMajor := math.Mod(p, major) == 0
		w := kindWeight("round")
		name := "round " + fmtPts(p)
		if isMajor {
			w *= 2
			name = "round " + fmtPts(p) + " (major)"
		}
		out = append(out, Level{Kind: "round", Name: name, Price: p, Source: SourceBars, Weight: w})
	}
	return out
}

func roundSteps(market string) (minor, major float64) {
	switch market {
	case "ES":
		return 25, 100
	case "NQ":
		return 100, 500
	case "YM":
		return 100, 500
	case "RTY":
		return 10, 50
	case "CL":
		return 1, 5
	case "GC":
		return 10, 50
	}
	return 10, 100
}

func fmtPts(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func shortDay(day string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	return t.Format("2 Jan")
}
