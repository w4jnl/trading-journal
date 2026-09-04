package handlers

import (
	"context"
	"net/url"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// openPositionRow is one open-position line with its link resolved for the
// viewer. The reconcile editor (/admin/transactions) is admin-only, so a
// non-admin gets the account's trade history for that instrument instead of
// a 403 "admin only" - the same IsAdmin predicate RequireAdmin uses, so an
// act-as session lands where the middleware would let it.
type openPositionRow struct {
	models.ListOpenPositionsRow
	Href  string
	Title string
}

// openPositionRows resolves the per-viewer links for the open_positions
// partial (dashboard + /admin/transactions).
func openPositionRows(ctx context.Context, rows []models.ListOpenPositionsRow) []openPositionRow {
	u := auth.UserFrom(ctx)
	admin := u != nil && u.IsAdmin
	out := make([]openPositionRow, 0, len(rows))
	for _, r := range rows {
		row := openPositionRow{ListOpenPositionsRow: r}
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
