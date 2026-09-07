package handlers

import (
	"context"
	"net/url"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

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
type openPositionRow struct {
	models.ListOpenPositionsRow
	Href     string
	Title    string
	AvgEntry *float64
}

// openPositionRows resolves the per-viewer links and the open average entry
// for the open_positions partial (dashboard + /admin/transactions). Nil-safe
// on h.Queries (link-only) so the partial's callers and tests need no DB.
func (h *Handlers) openPositionRows(ctx context.Context, rows []models.ListOpenPositionsRow) []openPositionRow {
	u := auth.UserFrom(ctx)
	admin := u != nil && u.IsAdmin
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
