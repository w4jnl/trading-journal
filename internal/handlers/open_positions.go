package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/topstepx"
)

// latestPriceTimeout bounds each quote lookup on a dashboard render; a
// provider slower than this is skipped in favour of the next one.
const latestPriceTimeout = 4 * time.Second

// quoteCacheTTL is how long a fetched quote is reused across renders. The
// dashboard re-renders on every data change (live refresh), so without this
// every import would fan out into vendor calls.
const quoteCacheTTL = 20 * time.Second

// quote is a priced moment from one of the providers.
type quote struct {
	Price  float64
	At     time.Time
	Source string // "TopstepX" | "Yahoo" | "cached bars"
}

type cachedQuote struct {
	q       quote
	fetched time.Time
}

// latestQuote prices a contract through the provider chain, memoized per
// symbol for quoteCacheTTL (copytrader accounts share contracts, and the
// dashboard re-renders on every data change):
//
//  1. TopstepX - the user's own session, History/retrieveBars for the exact
//     contract (CON.F.US.EP.U26), partial current minute included. Exchange
//     data, freshest; skipped silently when the user has no TopstepX
//     connection or the root has no ProjectX code.
//  2. Yahoo live quote for the root's front-month symbol (meta block of the
//     chart endpoint) - works even when Yahoo has no intraday bars for the
//     day, which is what left the dashboard showing Friday's close.
//  3. The newest cached bar (CachingSource.Quote falls back to it itself).
//
// nil when no provider answers within latestPriceTimeout each.
func (h *Handlers) latestQuote(ctx context.Context, u *auth.User, symbol, root string) *quote {
	if c, ok := h.quoteCache.Load(symbol); ok {
		if cq := c.(cachedQuote); time.Since(cq.fetched) < quoteCacheTTL {
			return &cq.q
		}
	}
	q := h.fetchQuote(ctx, u, symbol, root)
	if q != nil {
		h.quoteCache.Store(symbol, cachedQuote{q: *q, fetched: time.Now()})
	}
	return q
}

func (h *Handlers) fetchQuote(ctx context.Context, u *auth.User, symbol, root string) *quote {
	if u != nil && h.TopstepxSync != nil {
		if cid, ok := topstepx.ContractIDForSymbol(symbol, root, time.Now()); ok {
			tctx, cancel := context.WithTimeout(ctx, latestPriceTimeout)
			bar, err := h.TopstepxSync.LatestBar(tctx, u.ID, cid)
			cancel()
			switch {
			case err == nil:
				return &quote{Price: bar.Close, At: bar.Time, Source: "TopstepX"}
			case !errors.Is(err, topstepx.ErrNoCredentials):
				h.logger().Debug("open positions: topstepx quote unavailable", "contract", cid, "err", err)
			}
		}
	}
	if h.MarketData == nil {
		return nil
	}
	sym, ok := marketdata.YahooSymbol(root)
	if !ok {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, latestPriceTimeout)
	defer cancel()
	lq, err := h.MarketData.Quote(tctx, sym)
	if err != nil {
		h.logger().Debug("open positions: market-data quote unavailable", "root", root, "symbol", sym, "err", err)
		return nil
	}
	src := "Yahoo"
	if time.Since(lq.At) > 2*time.Hour {
		src = "cached bars"
	}
	return &quote{Price: lq.Price, At: lq.At, Source: src}
}

// instrument loads (once per root per render) the instrument row for tick
// specs; nil when unknown.
func (h *Handlers) instrument(ctx context.Context, root string, memo map[string]*models.Instrument) *models.Instrument {
	if inst, seen := memo[root]; seen {
		return inst
	}
	memo[root] = nil
	inst, err := h.rq().GetInstrument(ctx, root)
	if err != nil {
		return nil
	}
	memo[root] = &inst
	return &inst
}

// unrealizedPnL prices the open position at last: the move from the average
// entry in ticks, times the tick value, times the signed net quantity (long
// positive, short negative - so a short profits from a falling price). nil
// when the instrument lacks tick specs: a stub instrument shows no P&L rather
// than a price-difference guess, exactly like realized trades.
func unrealizedPnL(avg, last, netQty float64, inst models.Instrument) *float64 {
	if inst.TickSize == nil || inst.TickValue == nil || *inst.TickSize <= 0 || *inst.TickValue <= 0 || netQty == 0 {
		return nil
	}
	ticks := (last - avg) / *inst.TickSize
	pnl := ticks * *inst.TickValue * netQty
	return &pnl
}
