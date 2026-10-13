// Package levels replays stops and targets on the bars after an entry: the
// stop & target lab runs every past decision under a grid of fixed stops and
// targets (points from the entry) and an optional time stop, against what
// the trader's own exits made. Everything here is arithmetic; an AI only
// reads the result.
package levels

import "math"

// Snap rounds a price to the tick (unchanged without one).
func Snap(price, tick float64) float64 {
	if tick <= 0 {
		return round2(price)
	}
	return round2(math.Round(price/tick) * tick)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
