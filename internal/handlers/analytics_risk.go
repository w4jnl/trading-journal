package handlers

import (
	"math"
	"time"
)

// Drawdown and streak metrics over the filtered trades (P8). Both walk the
// trades in exit order, which is how fetchAnalyticsTrades returns them.

// drawdownMetrics describes the deepest peak-to-trough fall of the
// cumulative net P&L curve and where the curve stands now.
type drawdownMetrics struct {
	MaxDD        float64    `json:"maxDrawdown"`          // positive magnitude; 0 when never below a peak
	MaxDDPeakAt  *time.Time `json:"maxDrawdownPeakAt"`    // the high the fall started from
	MaxDDTrough  *time.Time `json:"maxDrawdownTroughAt"`  // the low
	MaxDDRecover *time.Time `json:"maxDrawdownRecoverAt"` // first exit at or above the peak again; nil while open
	MaxDDDays    int        `json:"maxDrawdownDays"`      // peak -> recovery (or -> last trade while open)
	CurrentDD    float64    `json:"currentDrawdown"`      // distance below the all-time peak now; 0 at a high
	Underwater   bool       `json:"underwater"`           // CurrentDD > 0
	PeakAt       *time.Time `json:"peakAt"`               // when the all-time peak was set
}

// computeDrawdown walks the cumulative net P&L trade by trade.
func computeDrawdown(trades []analyticsTrade) drawdownMetrics {
	var m drawdownMetrics
	var cum, peak float64
	var peakAt time.Time
	var fallStart time.Time // peak the current fall started from
	var curTrough float64
	var curTroughAt time.Time
	first := true
	for _, t := range trades {
		if t.NetPnl == nil {
			continue
		}
		cum += *t.NetPnl
		at := t.ExitTime
		if first {
			first = false
			peak, peakAt = cum, at
			if cum < 0 {
				// A losing first trade is a fall from the zero line.
				peak, peakAt = 0, at
			}
			fallStart = peakAt
			curTrough, curTroughAt = cum, at
		}
		if cum >= peak {
			// Back at (or above) the peak: the fall that ran from fallStart
			// is closed; record it if it was the deepest so far.
			if dd := peak - curTrough; dd > m.MaxDD && curTrough < peak {
				m.MaxDD = dd
				fs, ta, ra := fallStart, curTroughAt, at
				m.MaxDDPeakAt, m.MaxDDTrough, m.MaxDDRecover = &fs, &ta, &ra
				m.MaxDDDays = daysBetween(fs, ra)
			}
			peak, peakAt = cum, at
			fallStart = at
			curTrough, curTroughAt = cum, at
			continue
		}
		if cum < curTrough {
			curTrough, curTroughAt = cum, at
		}
	}
	if first {
		return m
	}
	// An open fall at the end may still be the deepest.
	if dd := peak - curTrough; dd > m.MaxDD && curTrough < peak {
		m.MaxDD = dd
		fs, ta := fallStart, curTroughAt
		m.MaxDDPeakAt, m.MaxDDTrough, m.MaxDDRecover = &fs, &ta, nil
		m.MaxDDDays = daysBetween(fs, trades[len(trades)-1].ExitTime)
	}
	m.CurrentDD = peak - cum
	if m.CurrentDD < 1e-9 {
		m.CurrentDD = 0
	}
	m.Underwater = m.CurrentDD > 0
	pa := peakAt
	m.PeakAt = &pa
	return m
}

func daysBetween(a, b time.Time) int {
	if b.Before(a) {
		a, b = b, a
	}
	return int(math.Round(b.Sub(a).Hours() / 24))
}

// streakMetrics counts consecutive wins and losses, by trade and by
// trading day. Current is signed: +3 = three wins running, -2 = two losses.
type streakMetrics struct {
	LongestWinTrades  int `json:"longestWinTrades"`
	LongestLossTrades int `json:"longestLossTrades"`
	CurrentTrades     int `json:"currentTrades"`
	LongestGreenDays  int `json:"longestGreenDays"`
	LongestRedDays    int `json:"longestRedDays"`
	CurrentDays       int `json:"currentDays"`
	// Magnitudes of the current runs, for the templates (html/template has
	// no abs for ints).
	CurrentTradesAbs int `json:"-"`
	CurrentDaysAbs   int `json:"-"`
}

// computeStreaks takes trades in exit order and the daily series in date
// order; zero-P&L trades and days break a streak without starting one.
func computeStreaks(trades []analyticsTrade, daily []dailyPoint) streakMetrics {
	var m streakMetrics
	m.LongestWinTrades, m.LongestLossTrades, m.CurrentTrades = runs(len(trades), func(i int) int {
		if trades[i].NetPnl == nil {
			return 0
		}
		return sign(*trades[i].NetPnl)
	})
	m.LongestGreenDays, m.LongestRedDays, m.CurrentDays = runs(len(daily), func(i int) int { return sign(daily[i].NetPnl) })
	m.CurrentTradesAbs, m.CurrentDaysAbs = absInt(m.CurrentTrades), absInt(m.CurrentDays)
	return m
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// runs scans a sequence of signs and returns the longest positive run,
// the longest negative run, and the signed length of the final run.
func runs(n int, signAt func(i int) int) (longestPos, longestNeg, current int) {
	for i := 0; i < n; i++ {
		s := signAt(i)
		switch {
		case s == 0:
			current = 0
		case s > 0 && current > 0:
			current++
		case s > 0:
			current = 1
		case s < 0 && current < 0:
			current--
		default:
			current = -1
		}
		if current > longestPos {
			longestPos = current
		}
		if -current > longestNeg {
			longestNeg = -current
		}
	}
	return longestPos, longestNeg, current
}

func sign(v float64) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}
