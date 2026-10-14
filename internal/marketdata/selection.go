package marketdata

import "time"

// SelectTimeframe chooses a DISPLAY timeframe and chart window for a trade (or a
// group of trades on one contract) spanning [firstEntry, lastExit]. It targets
// ~30 bars across the trade so a single bar never swallows the whole trade,
// pads the window with context on each side, then clamps to a coarser frame
// when the trade is too old for Yahoo to serve the finer one. now is injected so
// the function stays pure and testable. The returned window is [from, to] UTC.
func SelectTimeframe(firstEntry, lastExit, now time.Time) (Timeframe, time.Time, time.Time) {
	if lastExit.Before(firstEntry) {
		lastExit = firstEntry
	}
	span := lastExit.Sub(firstEntry)
	if span < 5*time.Minute {
		span = 5 * time.Minute // floor for scalps / open / instantaneous trades
	}

	// Aim for ~targetBars bars across the trade, picking the ladder rung whose
	// bar count lands closest to the target (by ratio, so over- and under-shoot
	// are weighed evenly). This avoids overshooting to 1d when the ideal width
	// falls in the 1h..1d gap - a 1.5-day swing wants 1h (36 bars), not 1d.
	tf := pickByBarCount(span)

	from, to := padWindow(firstEntry, lastExit, span, tf)

	// Degrade for age: step coarser until the window fits Yahoo's lookback.
	for now.Sub(from) > tfTable[tf].maxLookback {
		next := coarser(tf)
		if next == tf {
			break // already 1d
		}
		tf = next
		from, to = padWindow(firstEntry, lastExit, span, tf)
	}
	return tf, from, to
}

// padWindow adds context on each side of the trade and caps the total bar count.
func padWindow(firstEntry, lastExit time.Time, span time.Duration, tf Timeframe) (time.Time, time.Time) {
	pad := span + span/2 // 1.5 * span each side -> trade is the middle ~25%
	if minPad := 30 * tfTable[tf].dur; minPad > pad {
		pad = minPad
	}
	from := firstEntry.Add(-pad)
	to := lastExit.Add(pad)

	// Cap at ~600 bars so the payload stays small; shrink the pad symmetrically.
	if maxWin := 600 * tfTable[tf].dur; to.Sub(from) > maxWin {
		extra := to.Sub(from) - maxWin
		from = from.Add(extra / 2)
		to = to.Add(-extra / 2)
		if to.Before(lastExit) {
			to = lastExit
		}
		if from.After(firstEntry) {
			from = firstEntry
		}
	}
	return from, to
}

const targetBars = 30.0

// pickByBarCount returns the timeframe whose bar count across span is closest to
// targetBars, measured as a symmetric ratio so 60 bars and 15 bars are judged
// equally far from 30.
func pickByBarCount(span time.Duration) Timeframe {
	best := tfLadder[0]
	bestScore := barCountScore(span, best)
	for _, c := range tfLadder[1:] {
		if s := barCountScore(span, c); s < bestScore {
			bestScore = s
			best = c
		}
	}
	return best
}

func barCountScore(span time.Duration, tf Timeframe) float64 {
	bars := float64(span) / float64(tfTable[tf].dur)
	if bars <= 0 {
		return 1e18
	}
	if bars < targetBars {
		return targetBars / bars
	}
	return bars / targetBars
}

func coarser(tf Timeframe) Timeframe {
	if i := ladderIndex(tf); i >= 0 && i+1 < len(tfLadder) {
		return tfLadder[i+1]
	}
	return tf
}

// DayWindow is the whole trading day (or days) a trade spans, in the
// cache's day bucketing: from the start of firstEntry's day to the end of
// lastExit's day, clipped at now. The chart loads this much (the finest
// bars the cache holds, rolled up client-side) and focuses on the trade
// inside it, so the session around the trade is a scroll away instead of
// another fetch.
func DayWindow(firstEntry, lastExit, now time.Time, loc *time.Location) (time.Time, time.Time) {
	if lastExit.Before(firstEntry) {
		lastExit = firstEntry
	}
	from := startOfDay(firstEntry, loc)
	to := startOfDay(lastExit, loc).AddDate(0, 0, 1)
	if to.After(now) {
		to = now
	}
	return from, to
}
