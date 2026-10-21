package handlers

import (
	"context"
	"time"

	"github.com/w4jnl/trading-journal/internal/aimethod"
	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// ---- The session for the model ----------------------------------------
//
// Instead of ninety bars around the trade, the model gets the last
// sessions as a trader reads them: the structure (marketdata.ComputeSession
// over the cached one-minute bars: prior RTH, overnight, opening range,
// VWAP, ATR, swings) and a multi-timeframe window aggregated on read - two
// days of 15m, the last three hours of 5m, the last thirty minutes of 1m -
// all as of a moment, so a closed trade is judged with what was known at
// its entry. The warm-up (marketdata.Warmer) keeps the traded roots'
// recent days complete and fresh.

const (
	aiSessionDays = 3  // days of one-minute bars read from the cache
	aiBars15m     = 52 // about thirty hours
	aiBars5m      = 24 // three hours
	aiBars1m      = 20
)

// aiSession is the session context and window for a root as of asOf, nil
// without cached bars.
func (h *Handlers) aiSession(ctx context.Context, root string, asOf time.Time) map[string]any {
	if h.MarketData == nil {
		return nil
	}
	sym, ok := marketdata.YahooSymbol(root)
	if !ok {
		return nil
	}
	bars, tf, err := h.MarketData.RecentBars(ctx, sym, aiSessionDays, asOf)
	if err != nil || len(bars) == 0 {
		return nil
	}
	tz := h.localTZ()
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		chicago = tz
	}
	var in []marketdata.Bar
	for _, b := range bars {
		if time.Unix(b.Time, 0).Before(asOf) {
			in = append(in, b)
		}
	}
	if len(in) == 0 {
		return nil
	}
	out := map[string]any{"barsStoredAt": string(tf), "timezone": tz.String()}
	if tf == marketdata.TF1m {
		sc := marketdata.ComputeSession(in, asOf, chicago)
		out["structure"] = sc
		out["levelsNearest"] = sc.Levels()
	}
	windows := map[string]any{}
	for _, w := range []struct {
		tf marketdata.Timeframe
		n  int
	}{{marketdata.TF15m, aiBars15m}, {marketdata.TF5m, aiBars5m}, {marketdata.TF1m, aiBars1m}} {
		if w.tf.Duration() < tf.Duration() {
			continue // stored coarser than this window
		}
		agg := in
		if w.tf != tf {
			agg = marketdata.AggregateTo(in, w.tf)
		}
		if len(agg) > w.n {
			agg = agg[len(agg)-w.n:]
		}
		windows[string(w.tf)] = barRows(agg, "Mon 15:04", tz)
	}
	out["windows"] = windows
	out["windowNote"] = aimethod.Glossary["sessionAtEntry.windows"]
	return out
}
