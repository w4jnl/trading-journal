// Package levels proposes where a trade's stop and target could have gone,
// from what was known at the entry only: the price structure in the bars
// before it, their volatility, the trader's own record before it (how much
// heat winners took, how far they ran) and the gamma levels at the time. It
// also replays a stop and a target on the bars after an entry, which is how
// the proposed levels are checked against what happened and how the
// stop/target lab measures a rule over many trades. Everything here is
// arithmetic; an AI only chooses among the candidates and explains.
package levels

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// Candidate is one price a stop or a target could go at.
type Candidate struct {
	Kind   string  `json:"kind"`   // stop | target
	Price  float64 `json:"price"`  // snapped to the tick
	Points float64 `json:"points"` // distance from the entry, always positive
	Source string  `json:"source"` // structure | volatility | history | gamma
	Note   string  `json:"note"`   // what it is: "swing low 13:42", "call wall"
}

// History is the trader's record before the trade (in points per decision):
// winners' heat and best open profit percentiles over N earlier decisions.
type History struct {
	Scope       string   `json:"scope"` // what it covers: "strategy ORB", "ES long"
	N           int      `json:"n"`
	WinnerHeat  *float64 `json:"winnerHeatP80"`
	WinnerBest  *float64 `json:"winnerBestP50"`
	WinnerBest8 *float64 `json:"winnerBestP80"`
}

// GammaLevel is one gamma level at the entry, in futures prices.
type GammaLevel struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

// Input is everything known at the entry.
type Input struct {
	Direction string // long | short
	Entry     float64
	EntryTime int64 // unix seconds
	Tick      float64
	Before    []marketdata.Bar // bars before the entry, ascending
	Loc       *time.Location   // for the notes' clock times
	History   *History
	Gamma     []GammaLevel
}

// Tuning of the candidates: the bars the structure is read from, the pivot
// width, the volatility multiples, how far a gamma level may be and still
// count.
const (
	structureBars = 60
	rangeBars     = 30
	pivotWidth    = 2
	atrBars       = 14
	atrStop       = 1.5
	atrTarget     = 3
	gammaMaxPct   = 0.005
	// structureKeep is how many swing levels per side survive (the nearest).
	structureKeep = 3
)

// Candidates lists the stops (on the losing side) and targets (on the
// winning side) the input supports, nearest first within each kind.
func Candidates(in Input) []Candidate {
	long := in.Direction == "long"
	sign := 1.0 // a target is above a long's entry
	if !long {
		sign = -1
	}
	loc := in.Loc
	if loc == nil {
		loc = time.UTC
	}
	var out []Candidate
	add := func(kind string, price float64, source, note string) {
		price = Snap(price, in.Tick)
		d := (price - in.Entry) * sign // > 0 on the winning side
		if kind == "stop" {
			d = -d
		}
		if d <= 0 {
			return // not on its side of the entry
		}
		out = append(out, Candidate{Kind: kind, Price: price, Points: round2(d), Source: source, Note: note})
	}
	before := in.Before
	if len(before) > structureBars {
		before = before[len(before)-structureBars:]
	}
	tick := in.Tick
	if tick <= 0 {
		tick = 0.01
	}

	// Structure: swing pivots, and the recent range's extremes.
	for i := pivotWidth; i < len(before)-pivotWidth; i++ {
		b := before[i]
		clock := time.Unix(b.Time, 0).In(loc).Format("15:04")
		if isPivot(before, i, true) { // swing low
			if long {
				add("stop", b.Low-tick, "structure", "below the swing low "+clock)
			} else {
				add("target", b.Low, "structure", "the swing low "+clock)
			}
		}
		if isPivot(before, i, false) { // swing high
			if long {
				add("target", b.High, "structure", "the swing high "+clock)
			} else {
				add("stop", b.High+tick, "structure", "above the swing high "+clock)
			}
		}
	}
	if n := min(len(before), rangeBars); n > 0 {
		hi, lo := math.Inf(-1), math.Inf(1)
		for _, b := range before[len(before)-n:] {
			hi, lo = math.Max(hi, b.High), math.Min(lo, b.Low)
		}
		label := fmt.Sprintf("the last %d bars' ", n)
		if long {
			add("stop", lo-tick, "structure", "below "+label+"low")
			add("target", hi, "structure", label+"high")
		} else {
			add("stop", hi+tick, "structure", "above "+label+"high")
			add("target", lo, "structure", label+"low")
		}
	}

	// Volatility: the average true range of the last bars.
	if atr := averageTrueRange(before, atrBars); atr > 0 {
		add("stop", in.Entry-sign*atrStop*atr, "volatility", fmt.Sprintf("%.1f x the %d-bar average range (%.2f pts)", atrStop, atrBars, atr))
		add("target", in.Entry+sign*atrTarget*atr, "volatility", fmt.Sprintf("%.0f x the %d-bar average range", float64(atrTarget), atrBars))
	}

	// History: where the trader's earlier winners went.
	if h := in.History; h != nil && h.N > 0 {
		if h.WinnerHeat != nil && *h.WinnerHeat > 0 {
			add("stop", in.Entry-sign*(*h.WinnerHeat+tick), "history",
				fmt.Sprintf("beyond the heat 80%% of your winners took (%s, %d decisions)", h.Scope, h.N))
		}
		if h.WinnerBest != nil && *h.WinnerBest > 0 {
			add("target", in.Entry+sign**h.WinnerBest, "history", fmt.Sprintf("half of your winners got this far (%s)", h.Scope))
		}
		if h.WinnerBest8 != nil && *h.WinnerBest8 > 0 {
			add("target", in.Entry+sign**h.WinnerBest8, "history", fmt.Sprintf("one in five of your winners got this far (%s)", h.Scope))
		}
	}

	// Gamma: a level near enough is a magnet ahead or a shelf behind.
	for _, g := range in.Gamma {
		if g.Price <= 0 || math.Abs(g.Price-in.Entry)/in.Entry > gammaMaxPct {
			continue
		}
		if (g.Price-in.Entry)*sign > 0 {
			add("target", g.Price, "gamma", g.Name)
		} else {
			add("stop", g.Price-sign*tick, "gamma", "beyond the "+g.Name)
		}
	}

	out = dedupe(out, tick)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "stop"
		}
		return out[i].Points < out[j].Points
	})
	// Only the nearest few swing levels per side: farther ones are noise
	// next to the range, volatility, history and gamma candidates.
	kept := out[:0]
	swings := map[string]int{}
	for _, c := range out {
		if c.Source == "structure" && strings.Contains(c.Note, "swing") {
			if swings[c.Kind] == structureKeep {
				continue
			}
			swings[c.Kind]++
		}
		kept = append(kept, c)
	}
	return kept
}

// isPivot reports a swing low (low) or high at i: lower (higher) than the
// pivotWidth bars on each side.
func isPivot(bars []marketdata.Bar, i int, low bool) bool {
	for k := 1; k <= pivotWidth; k++ {
		if low && (bars[i].Low >= bars[i-k].Low || bars[i].Low >= bars[i+k].Low) {
			return false
		}
		if !low && (bars[i].High <= bars[i-k].High || bars[i].High <= bars[i+k].High) {
			return false
		}
	}
	return true
}

// averageTrueRange over the last n bars (0 without two bars).
func averageTrueRange(bars []marketdata.Bar, n int) float64 {
	if len(bars) < 2 {
		return 0
	}
	start := max(1, len(bars)-n)
	sum, cnt := 0.0, 0
	for i := start; i < len(bars); i++ {
		prev := bars[i-1].Close
		tr := math.Max(bars[i].High-bars[i].Low, math.Max(math.Abs(bars[i].High-prev), math.Abs(bars[i].Low-prev)))
		sum += tr
		cnt++
	}
	return sum / float64(cnt)
}

// dedupe keeps one candidate per kind and price (the first), dropping a
// candidate within a tick of one already kept.
func dedupe(in []Candidate, tick float64) []Candidate {
	var out []Candidate
	for _, c := range in {
		dup := false
		for _, k := range out {
			if k.Kind == c.Kind && math.Abs(k.Price-c.Price) < tick/2 {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// Snap rounds a price to the tick (unchanged without one).
func Snap(price, tick float64) float64 {
	if tick <= 0 {
		return round2(price)
	}
	return round2(math.Round(price/tick) * tick)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
