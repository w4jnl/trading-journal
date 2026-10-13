package levels

import (
	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// Outcome is what a stop and a target did on the bars after an entry.
type Outcome struct {
	// Hit: target, stop, both (the same bar reached both, so which came
	// first is unknown - counted as the stop by a cautious reader), neither
	// within the bars, or nodata (no bar after the entry's own).
	Hit     string  `json:"hit"`
	At      int64   `json:"at,omitempty"` // the deciding bar's time
	Points  float64 `json:"points"`       // result: +target, -stop, or the last close for neither
	Minutes float64 `json:"minutes"`      // from the entry to the deciding bar (or the last bar)
}

// Replay walks the bars after the entry's own bar (that bar straddles the
// entry, so its range is not all after it) until one reaches the stop or the
// target; stop or target 0 means none on that side. after must be ascending
// and may start anywhere: bars at or before the entry bar are skipped.
func Replay(after []marketdata.Bar, direction string, entry, stop, target float64, entryTime int64) Outcome {
	long := direction == "long"
	sign := 1.0
	if !long {
		sign = -1
	}
	// Skip through the bar that holds the entry (it opened at or before it).
	start := 0
	for start < len(after) && after[start].Time <= entryTime {
		start++
	}
	var last *marketdata.Bar
	for i := start; i < len(after); i++ {
		b := after[i]
		last = &after[i]
		hitStop := stop > 0 && ((long && b.Low <= stop) || (!long && b.High >= stop))
		hitTarget := target > 0 && ((long && b.High >= target) || (!long && b.Low <= target))
		mins := float64(b.Time-entryTime) / 60
		switch {
		case hitStop && hitTarget:
			return Outcome{Hit: "both", At: b.Time, Points: round2(-(entry - stop) * sign), Minutes: round2(mins)}
		case hitStop:
			return Outcome{Hit: "stop", At: b.Time, Points: round2(-(entry - stop) * sign), Minutes: round2(mins)}
		case hitTarget:
			return Outcome{Hit: "target", At: b.Time, Points: round2((target - entry) * sign), Minutes: round2(mins)}
		}
	}
	if last == nil {
		return Outcome{Hit: "nodata"}
	}
	return Outcome{Hit: "neither", Points: round2((last.Close - entry) * sign), Minutes: round2(float64(last.Time-entryTime) / 60)}
}
