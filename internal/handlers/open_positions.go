package handlers

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"errors"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
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

// openPositionRow is one open-position line with its link resolved for the
// viewer. The reconcile editor (/admin/transactions) is admin-only, so a
// non-admin gets the account's trade history for that instrument instead of
// a 403 "admin only" - the same IsAdmin predicate RequireAdmin uses, so an
// act-as session lands where the middleware would let it.
//
// AvgEntry is the volume-weighted average price of the lots still open,
// computed by replaying the account/contract's fills with the builder's FIFO
// rule (positions.OpenFromFills) - so after a partial close it reflects the
// contracts actually left, not every fill ever made. Nil when the fills could
// not be loaded or the replay nets flat despite the SQL net (data gap).
//
// LastPrice/LastAt/PriceSource is the freshest price a provider could give
// for the contract - the user's own TopstepX feed (exchange data, current
// minute) when they have one, else Yahoo's live quote for the root's front
// month, else the newest cached bar - and Unrealized the pending P&L at that
// price: (last - avg) in ticks x tick value x signed net qty, so a short
// gains when price falls. Nil when no provider answers or the instrument has
// no tick specs (stub awaiting setup) - the same rule as realized P&L, never
// a price-difference guess.
type openPositionRow struct {
	models.ListOpenPositionsRow
	Href        string
	Title       string
	AvgEntry    *float64
	LastPrice   *float64
	LastAt      *time.Time
	PriceSource string
	Unrealized  *float64
}

// openPositionRows resolves the per-viewer links, the open average entry and
// the pending P&L for the open_positions partial (dashboard +
// /admin/transactions). Nil-safe on h.Queries / h.MarketData / h.TopstepxSync
// (link-only, then avg-only) so the partial's callers and tests need no DB
// or vendor.
func (h *Handlers) openPositionRows(ctx context.Context, rows []models.ListOpenPositionsRow) []openPositionRow {
	u := auth.UserFrom(ctx)
	admin := u != nil && u.IsAdmin
	insts := map[string]*models.Instrument{}
	out := make([]openPositionRow, 0, len(rows))
	for _, r := range rows {
		row := openPositionRow{ListOpenPositionsRow: r}
		if h != nil && h.Queries != nil {
			fills, err := h.Queries.ListExecutionsForAccountSymbol(ctx, models.ListExecutionsForAccountSymbolParams{AccountID: r.AccountID, Symbol: r.Symbol})
			if err != nil {
				h.logger().Warn("open positions: load fills for average entry", "account", r.AccountID, "symbol", r.Symbol, "err", err)
			} else if pos := positions.OpenFromFills(fills); pos.NetQty != 0 {
				avg := pos.AvgPrice
				row.AvgEntry = &avg
				if q := h.latestQuote(ctx, u, r.Symbol, r.SymbolRoot); q != nil {
					price, at := q.Price, q.At
					row.LastPrice, row.LastAt, row.PriceSource = &price, &at, q.Source
					if inst := h.instrument(ctx, r.SymbolRoot, insts); inst != nil {
						row.Unrealized = unrealizedPnL(avg, q.Price, pos.NetQty, *inst)
					}
				}
			}
		}
		acct := strconv.FormatInt(r.AccountID, 10)
		if admin {
			q := url.Values{"account_id": {acct}, "symbol": {r.Symbol}}
			row.Href = "/admin/transactions?" + q.Encode()
			row.Title = "Reconcile this account's " + r.Symbol + " executions"
		} else {
			q := url.Values{"filter": {"acc:" + acct}, "symbol_root": {r.SymbolRoot}}
			row.Href = "/trades?" + q.Encode()
			row.Title = "Closed " + r.SymbolRoot + " trades on this account (reconciling executions is admin-only)"
		}
		out = append(out, row)
	}
	return out
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
	inst, err := h.Queries.GetInstrument(ctx, root)
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
