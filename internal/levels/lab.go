package levels

import (
	"math"
	"math/rand"
	"sort"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// The stop/target lab: every past decision replayed under a grid of fixed
// stops and targets (points from the entry) and an optional time stop, to
// see which rule would have paid - against what the trader's own exits made.

// LabDecision is one past decision with the bars after its entry.
type LabDecision struct {
	Direction string
	Entry     float64
	EntryTime int64            // unix seconds
	After     []marketdata.Bar // bars after the entry, ascending
	Actual    float64          // what the trader's exit made, in points
}

// Cell is one (stop, target) rule over the decisions.
type Cell struct {
	Stop       float64 `json:"stop"`   // points
	Target     float64 `json:"target"` // points; 0 = none (the stop or the time stop decides)
	N          int     `json:"n"`
	Wins       int     `json:"wins"` // ended up
	WinRate    float64 `json:"winRate"`
	Avg        float64 `json:"avgPts"` // expectancy, points per decision
	Total      float64 `json:"totalPts"`
	CILow      float64 `json:"ciLow"`      // 90% bootstrap interval of Avg
	CIHigh     float64 `json:"ciHigh"`     //
	Both       int     `json:"both"`       // decided by a bar that reached both (counted as the stop)
	Unresolved int     `json:"unresolved"` // neither level within the window (closed at its end)
}

// labResample is the bootstrap's resample count; the seed is fixed so the
// same grid reads the same twice.
const labResample = 400

// Grid replays every decision under every (stop, target) pair. timeStop > 0
// closes a decision still open after that many minutes at that bar's close.
func Grid(ds []LabDecision, stops, targets []float64, timeStop int) [][]Cell {
	rng := rand.New(rand.NewSource(1))
	out := make([][]Cell, len(stops))
	for i, st := range stops {
		out[i] = make([]Cell, len(targets))
		for j, tg := range targets {
			c := Cell{Stop: st, Target: tg}
			res := make([]float64, 0, len(ds))
			for _, d := range ds {
				after := d.After
				if timeStop > 0 {
					after = cutAfter(after, d.EntryTime+int64(timeStop)*60)
				}
				sign := 1.0
				if d.Direction != "long" {
					sign = -1
				}
				target := 0.0
				if tg > 0 {
					target = d.Entry + sign*tg
				}
				o := Replay(after, d.Direction, d.Entry, d.Entry-sign*st, target, d.EntryTime)
				if o.Hit == "nodata" {
					continue // nothing to replay on
				}
				switch o.Hit {
				case "both":
					c.Both++
				case "neither":
					c.Unresolved++
				}
				res = append(res, o.Points)
			}
			c.N = len(res)
			if c.N > 0 {
				for _, p := range res {
					c.Total += p
					if p > 0 {
						c.Wins++
					}
				}
				c.Avg = c.Total / float64(c.N)
				c.WinRate = float64(c.Wins) / float64(c.N)
				c.CILow, c.CIHigh = bootstrap(res, rng)
				c.Avg, c.Total, c.WinRate = round2(c.Avg), round2(c.Total), math.Round(c.WinRate*1000)/1000
			}
			out[i][j] = c
		}
	}
	return out
}

// cutAfter keeps the bars that open before the time stop.
func cutAfter(bars []marketdata.Bar, until int64) []marketdata.Bar {
	i := sort.Search(len(bars), func(i int) bool { return bars[i].Time >= until })
	return bars[:i]
}

// bootstrap is the 90% interval of the mean of xs by resampling.
func bootstrap(xs []float64, rng *rand.Rand) (lo, hi float64) {
	if len(xs) < 2 {
		m := xs[0]
		return round2(m), round2(m)
	}
	means := make([]float64, labResample)
	for r := range means {
		sum := 0.0
		for range xs {
			sum += xs[rng.Intn(len(xs))]
		}
		means[r] = sum / float64(len(xs))
	}
	sort.Float64s(means)
	return round2(means[int(0.05*float64(labResample))]), round2(means[int(0.95*float64(labResample))-1])
}

// Baseline is the trader's own exits over the same decisions.
type Baseline struct {
	N       int     `json:"n"`
	Avg     float64 `json:"avgPts"`
	WinRate float64 `json:"winRate"`
	CILow   float64 `json:"ciLow"`
	CIHigh  float64 `json:"ciHigh"`
}

// Actuals summarizes what the decisions' own exits made.
func Actuals(ds []LabDecision) Baseline {
	var b Baseline
	xs := make([]float64, 0, len(ds))
	for _, d := range ds {
		xs = append(xs, d.Actual)
	}
	if len(xs) == 0 {
		return b
	}
	wins := 0
	sum := 0.0
	for _, x := range xs {
		sum += x
		if x > 0 {
			wins++
		}
	}
	b.N, b.Avg = len(xs), round2(sum/float64(len(xs)))
	b.WinRate = math.Round(float64(wins)/float64(len(xs))*1000) / 1000
	b.CILow, b.CIHigh = bootstrap(xs, rand.New(rand.NewSource(1)))
	return b
}

// Spread returns up to n distinct values of xs at evenly spaced percentiles
// between lo and hi (0..1), snapped to the tick and positive: a grid axis
// from the trader's own heat or best-seen distribution.
func Spread(xs []float64, lo, hi float64, n int, tick float64) []float64 {
	if len(xs) == 0 || n <= 0 {
		return nil
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	var out []float64
	for k := 0; k < n; k++ {
		p := lo
		if n > 1 {
			p = lo + (hi-lo)*float64(k)/float64(n-1)
		}
		v := Snap(s[int(math.Round(p*float64(len(s)-1)))], tick)
		if v <= 0 {
			v = math.Max(tick, 0.25)
		}
		if len(out) == 0 || v > out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
