package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const tradesPageSize = 50

type tradeFilters struct {
	AccountID  *int64
	SymbolRoot *string
	Direction  *string
	From       *time.Time
	To         *time.Time
	Page       int
}

// tradeRow mirrors models.ListTradesFilteredRow, but we hand-scan it because
// sqlc + sqlc.narg generates broken positional indexing for SQLite (each narg
// becomes ?N which doesn't line up with the variadic positional args). The
// fields are exported so html/template can read them.
type tradeRow struct {
	ID          int64
	AccountID   int64
	Symbol      string
	SymbolRoot  string
	Direction   string
	Qty         float64
	EntryPrice  float64
	ExitPrice   *float64
	EntryTime   time.Time
	ExitTime    *time.Time
	GrossPnl    *float64
	Commission  *float64
	NetPnl      *float64
	Status      string
	AccountName string
	AccountFirm *string
}

// TradesPage renders /trades — a paginated, filterable table of closed trades.
func (h *Handlers) TradesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filters := parseTradeFilters(r)

	rows, err := listTradesFiltered(ctx, h.DB, filters)
	if err != nil {
		h.serverError(w, fmt.Errorf("list trades: %w", err))
		return
	}
	total, err := countTradesFiltered(ctx, h.DB, filters)
	if err != nil {
		h.serverError(w, fmt.Errorf("count trades: %w", err))
		return
	}

	accounts, err := h.Queries.ListAccounts(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	roots, err := h.Queries.ListDistinctSymbolRoots(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list roots: %w", err))
		return
	}

	pages := int((total + tradesPageSize - 1) / tradesPageSize)
	if pages < 1 {
		pages = 1
	}

	h.renderPage(w, "trades.html", map[string]any{
		"Trades":      rows,
		"Total":       total,
		"Page":        filters.Page,
		"Pages":       pages,
		"PageSize":    tradesPageSize,
		"Accounts":    accounts,
		"SymbolRoots": roots,
		"Filters":     filtersAsForm(filters),
		"PrevPage":    pageRef(r, filters.Page-1, pages),
		"NextPage":    pageRef(r, filters.Page+1, pages),
	})
}

// TradeDetail renders the trade detail partial — used by HTMX to populate a
// modal with the trade summary and its execution breakdown.
func (h *Handlers) TradeDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	trade, err := h.Queries.GetTradeByID(ctx, id)
	if err != nil {
		http.Error(w, "trade not found", http.StatusNotFound)
		return
	}
	execs, err := h.Queries.ListExecutionsForTrade(ctx, id)
	if err != nil {
		h.serverError(w, fmt.Errorf("list executions: %w", err))
		return
	}
	h.renderPartial(w, "_trade_detail.html", "trade_detail", map[string]any{
		"Trade":      trade,
		"Executions": execs,
	})
}

// listTradesFiltered + countTradesFiltered build a SQL WHERE clause from
// optional filter fields. Each filter contributes a "AND col = ?" clause and
// pushes its value onto the args slice — straightforward, no narg trickery.
func listTradesFiltered(ctx context.Context, db *sql.DB, f tradeFilters) ([]tradeRow, error) {
	where, args := buildWhere(f)
	args = append(args, tradesPageSize, (f.Page-1)*tradesPageSize)

	q := `SELECT t.id, t.account_id, t.symbol, t.symbol_root, t.direction, t.qty,
	             t.entry_price, t.exit_price, t.entry_time, t.exit_time,
	             t.gross_pnl, t.commission, t.net_pnl, t.status,
	             a.name, a.firm
	      FROM trades t
	      JOIN accounts a ON a.id = t.account_id
	      WHERE ` + where + `
	      ORDER BY t.entry_time DESC, t.id DESC
	      LIMIT ? OFFSET ?`

	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var out []tradeRow
	for rs.Next() {
		var r tradeRow
		if err := rs.Scan(
			&r.ID, &r.AccountID, &r.Symbol, &r.SymbolRoot, &r.Direction, &r.Qty,
			&r.EntryPrice, &r.ExitPrice, &r.EntryTime, &r.ExitTime,
			&r.GrossPnl, &r.Commission, &r.NetPnl, &r.Status,
			&r.AccountName, &r.AccountFirm,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func countTradesFiltered(ctx context.Context, db *sql.DB, f tradeFilters) (int64, error) {
	where, args := buildWhere(f)
	q := `SELECT COUNT(*) FROM trades t WHERE ` + where
	var n int64
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// buildWhere returns the dynamic WHERE clause and the matching args slice.
// Always includes status='closed' so open positions don't show up in the trade
// log (open positions have no P&L and aren't a useful unit of analysis).
func buildWhere(f tradeFilters) (string, []any) {
	clauses := []string{"t.status = 'closed'"}
	var args []any
	if f.AccountID != nil {
		clauses = append(clauses, "t.account_id = ?")
		args = append(args, *f.AccountID)
	}
	if f.SymbolRoot != nil {
		clauses = append(clauses, "t.symbol_root = ?")
		args = append(args, *f.SymbolRoot)
	}
	if f.Direction != nil {
		clauses = append(clauses, "t.direction = ?")
		args = append(args, *f.Direction)
	}
	if f.From != nil {
		clauses = append(clauses, "t.exit_time >= ?")
		args = append(args, *f.From)
	}
	if f.To != nil {
		clauses = append(clauses, "t.exit_time < ?")
		args = append(args, *f.To)
	}
	return strings.Join(clauses, " AND "), args
}

// parseTradeFilters reads filter values from the query string. Empty values
// are treated as "not filtering" — they map to nil pointers.
func parseTradeFilters(r *http.Request) tradeFilters {
	q := r.URL.Query()
	f := tradeFilters{Page: 1}

	if v := strings.TrimSpace(q.Get("account_id")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.AccountID = &id
		}
	}
	if v := strings.TrimSpace(q.Get("symbol_root")); v != "" {
		f.SymbolRoot = &v
	}
	if v := strings.TrimSpace(q.Get("direction")); v == "long" || v == "short" {
		f.Direction = &v
	}
	if v := strings.TrimSpace(q.Get("from")); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			f.From = &t
		}
	}
	if v := strings.TrimSpace(q.Get("to")); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			t = t.Add(24 * time.Hour) // inclusive upper bound
			f.To = &t
		}
	}
	if v := strings.TrimSpace(q.Get("page")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p >= 1 {
			f.Page = p
		}
	}
	return f
}

// filtersAsForm flattens the filter struct back into string fields for the
// template (so the form re-renders the active filters).
func filtersAsForm(f tradeFilters) map[string]string {
	out := map[string]string{}
	if f.AccountID != nil {
		out["AccountID"] = strconv.FormatInt(*f.AccountID, 10)
	}
	if f.SymbolRoot != nil {
		out["SymbolRoot"] = *f.SymbolRoot
	}
	if f.Direction != nil {
		out["Direction"] = *f.Direction
	}
	if f.From != nil {
		out["From"] = f.From.Format("2006-01-02")
	}
	if f.To != nil {
		out["To"] = f.To.Add(-24 * time.Hour).Format("2006-01-02")
	}
	return out
}

// pageRef returns a query string for prev/next-page links, preserving filters.
func pageRef(r *http.Request, page, pages int) string {
	if page < 1 || page > pages {
		return ""
	}
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page))
	return "?" + q.Encode()
}
