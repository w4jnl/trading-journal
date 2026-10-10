package handlers

import (
	"testing"
	"time"
)

func atTrades(nets ...float64) []analyticsTrade {
	out := make([]analyticsTrade, len(nets))
	base := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	for i, n := range nets {
		v := n
		out[i] = analyticsTrade{ID: int64(i + 1), NetPnl: &v, ExitTime: base.AddDate(0, 0, i)}
	}
	return out
}

// Cumulative: 100, 150, 50, -50, 0, 200, 150. Deepest fall 150 -> -50 =
// 200, from day 2 to the trough on day 4, recovered on day 6 (200 >= 150);
// the curve ends 50 below the new peak of 200.
func TestComputeDrawdown(t *testing.T) {
	dd := computeDrawdown(atTrades(100, 50, -100, -100, 50, 200, -50))
	if dd.MaxDD != 200 {
		t.Fatalf("max drawdown = %v, want 200", dd.MaxDD)
	}
	if dd.MaxDDPeakAt == nil || dd.MaxDDPeakAt.Day() != 2 || dd.MaxDDTrough == nil || dd.MaxDDTrough.Day() != 4 || dd.MaxDDRecover == nil || dd.MaxDDRecover.Day() != 6 {
		t.Errorf("drawdown dates: peak=%v trough=%v recover=%v", dd.MaxDDPeakAt, dd.MaxDDTrough, dd.MaxDDRecover)
	}
	if dd.MaxDDDays != 4 {
		t.Errorf("drawdown days = %d, want 4", dd.MaxDDDays)
	}
	if dd.CurrentDD != 50 || !dd.Underwater {
		t.Errorf("current drawdown = %v underwater=%v, want 50 / true", dd.CurrentDD, dd.Underwater)
	}
	// Still-open fall counts, and a first losing trade falls from zero.
	open := computeDrawdown(atTrades(-100, -50, 20))
	if open.MaxDD != 150 || open.MaxDDRecover != nil || open.CurrentDD != 130 {
		t.Errorf("open drawdown: %+v", open)
	}
	if up := computeDrawdown(atTrades(10, 20, 30)); up.MaxDD != 0 || up.Underwater {
		t.Errorf("monotone curve has a drawdown: %+v", up)
	}
	if z := computeDrawdown(nil); z.PeakAt != nil || z.MaxDD != 0 {
		t.Errorf("empty: %+v", z)
	}
}

func TestComputeStreaks(t *testing.T) {
	trades := atTrades(10, 20, -5, -5, -5, 0, 7, 8)
	daily := []dailyPoint{{NetPnl: 5}, {NetPnl: 5}, {NetPnl: 5}, {NetPnl: -1}, {NetPnl: 0}, {NetPnl: -2}, {NetPnl: -2}}
	s := computeStreaks(trades, daily)
	if s.LongestWinTrades != 2 || s.LongestLossTrades != 3 || s.CurrentTrades != 2 {
		t.Errorf("trade streaks: %+v", s)
	}
	if s.LongestGreenDays != 3 || s.LongestRedDays != 2 || s.CurrentDays != -2 {
		t.Errorf("day streaks: %+v", s)
	}
}
