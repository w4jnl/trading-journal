package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

const tradesPageSize = 50

type tradeFilters struct {
	// Account is the resolved union of acc:N / type:X selections.
	Account    accountFilter
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
//
// HasJournal/JournalTag come from a LEFT JOIN onto journal_entries so /trades
// rows can flag which trades already have an entry. There's at most one
// trade-level entry per trade in v1.
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
	HasJournal  bool
	JournalTag  string // "" if no entry or no tag set
}

// TradesPage renders /trades — a paginated, filterable table of closed trades.
func (h *Handlers) TradesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filters, err := h.parseTradeFilters(r)
	if err != nil {
		h.serverError(w, fmt.Errorf("parse filters: %w", err))
		return
	}

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

	// Chart data: only when the user has narrowed to a date range (calendar
	// day cell, Saturday week link, or hand-set from/to). Outside that case
	// the page is the global trade log — a cumulative curve over hundreds of
	// trades is not useful, so we skip rendering the chart entirely.
	chartXs, chartYs, chartTitle, chartTotal, err := buildPerfChart(ctx, h.DB, filters)
	if err != nil {
		h.serverError(w, fmt.Errorf("perf chart: %w", err))
		return
	}

	// Per-trade running balance — populated only for trades whose account has
	// at least one balance anchor on /accounts. Other rows render "—".
	tradeBalances, err := h.loadTradeBalances(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("trade balances: %w", err))
		return
	}

	h.renderPage(w, "trades.html", map[string]any{
		"NavActive":        "trades",
		"BackHref":         validBackHref(r),
		"Trades":           rows,
		"Total":            total,
		"Page":             filters.Page,
		"Pages":            pages,
		"PageSize":         tradesPageSize,
		"Accounts":         accounts,
		"SymbolRoots":      roots,
		"Filters":          filtersAsForm(filters),
		"FilterSelections": filters.Account.Selected,
		"FilterCount":      filters.Account.SelectedCount,
		"PrevPage":         pageRef(r, filters.Page-1, pages),
		"NextPage":         pageRef(r, filters.Page+1, pages),
		"ChartXs":          chartXs,    // []int64 Unix timestamps (seconds)
		"ChartYs":          chartYs,    // []float64 cumulative net P&L
		"ChartTitle":       chartTitle, // empty string when chart is suppressed
		"ChartTotal":       chartTotal, // final cumulative — drives stroke colour
		"TradeBalances":    tradeBalances, // map[trade_id]float64, missing = no anchor
	})
}

// buildPerfChart returns the cumulative-P&L curve points for the active
// filters when from/to are both set. Returned arrays are empty when the
// chart should be suppressed (no date range or zero matching trades).
//
// The cumulative is computed over EVERY filtered trade (not just the visible
// page), ordered by exit_time ASC. The series prepends a (firstExitTime-1min,
// 0) baseline point so the curve starts at zero and ramps to the first trade's
// P&L — matches the visual in exportData/performance.jpg.
func buildPerfChart(ctx context.Context, db *sql.DB, f tradeFilters) (xs []int64, ys []float64, title string, total float64, err error) {
	if f.From == nil || f.To == nil {
		return nil, nil, "", 0, nil
	}
	where, args := buildWhere(f)
	q := `SELECT t.exit_time, t.net_pnl
	      FROM trades t
	      WHERE ` + where + `
	        AND t.exit_time IS NOT NULL
	        AND t.net_pnl   IS NOT NULL
	      ORDER BY t.exit_time ASC, t.id ASC`
	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, "", 0, err
	}
	defer rs.Close()
	type point struct {
		t   time.Time
		net float64
	}
	var raw []point
	for rs.Next() {
		var t time.Time
		var net float64
		if err := rs.Scan(&t, &net); err != nil {
			return nil, nil, "", 0, err
		}
		raw = append(raw, point{t, net})
	}
	if err := rs.Err(); err != nil {
		return nil, nil, "", 0, err
	}
	if len(raw) == 0 {
		return nil, nil, "", 0, nil
	}
	// Prepend a baseline point at first exit minus 1 minute so the line
	// starts at zero and rises to the first trade's net.
	xs = make([]int64, 0, len(raw)+1)
	ys = make([]float64, 0, len(raw)+1)
	xs = append(xs, raw[0].t.Add(-time.Minute).Unix())
	ys = append(ys, 0)
	cum := 0.0
	for _, p := range raw {
		cum += p.net
		xs = append(xs, p.t.Unix())
		ys = append(ys, cum)
	}
	total = cum

	fromStr := f.From.Format("2006-01-02")
	toStr := f.To.Add(-24 * time.Hour).Format("2006-01-02") // To is exclusive +24h; restore the user-facing day
	if fromStr == toStr {
		title = "Day Performance · " + fromStr
	} else {
		title = "Range Performance · " + fromStr + " – " + toStr
	}
	return xs, ys, title, total, nil
}

// TradeDetail renders the trade detail partial — used by HTMX to populate a
// modal with the trade summary, its execution breakdown, and its journal entry.
func (h *Handlers) TradeDetail(w http.ResponseWriter, r *http.Request) {
	h.renderTradeDetail(w, r, "")
}

// renderTradeDetail is the shared rendering path used by both GET /trades/{id}
// and POST /trades/{id}/journal (so the modal refreshes in place after a save).
func (h *Handlers) renderTradeDetail(w http.ResponseWriter, r *http.Request, flash string) {
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

	// Trade-level journal entry, if one exists.
	var journalBody, journalTag string
	var hasJournal bool
	var journalRendered template.HTML
	if entry, err := h.Queries.GetJournalEntryByTradeID(ctx, &id); err == nil {
		hasJournal = true
		journalBody = entry.Body
		if entry.Tag != nil {
			journalTag = *entry.Tag
		}
		journalRendered = template.HTML(renderMarkdown(entry.Body))
	} else if !errors.Is(err, sql.ErrNoRows) {
		h.serverError(w, fmt.Errorf("get trade journal: %w", err))
		return
	}

	h.renderPartial(w, "_trade_detail.html", "trade_detail", map[string]any{
		"Trade":           trade,
		"TradeID":         id,
		"Executions":      execs,
		"HasJournal":      hasJournal,
		"JournalBody":     journalBody,
		"JournalTag":      journalTag,
		"JournalRendered": journalRendered,
		"Flash":           flash,
	})
}

// TradeJournalSave handles POST /trades/{id}/journal. Empty body deletes the
// entry. On success it returns the refreshed trade detail partial so the
// HTMX-swapped modal updates in place.
func (h *Handlers) TradeJournalSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	tradeID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	body := strings.TrimSpace(r.FormValue("body"))
	tag := parseTag(r.FormValue("tag"))

	// Use the trade's exit date (or entry date if still open) so day-by-date
	// queries pick this up alongside the trade.
	trade, err := h.Queries.GetTradeByID(ctx, tradeID)
	if err != nil {
		http.Error(w, "trade not found", http.StatusNotFound)
		return
	}
	entryDate := trade.EntryTime
	if trade.ExitTime != nil {
		entryDate = *trade.ExitTime
	}

	existing, err := h.Queries.GetJournalEntryByTradeID(ctx, &tradeID)
	flash := "saved"
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if body == "" {
			flash = "empty"
			break
		}
		if _, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			TradeID:   &tradeID,
			AccountID: &trade.AccountID,
			EntryDate: entryDate,
			Body:      body,
			Mood:      nil,
			Tag:       tag,
			Scope:     "trade",
		}); err != nil {
			h.serverError(w, fmt.Errorf("create trade journal: %w", err))
			return
		}
	case err != nil:
		h.serverError(w, fmt.Errorf("get trade journal: %w", err))
		return
	default:
		if body == "" {
			if err := h.Queries.DeleteJournalEntry(ctx, existing.ID); err != nil {
				h.serverError(w, fmt.Errorf("delete trade journal: %w", err))
				return
			}
			flash = "deleted"
		} else {
			if err := h.Queries.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
				Body: body,
				Mood: existing.Mood, // preserve mood; trade entries don't expose it in v1
				Tag:  tag,
				ID:   existing.ID,
			}); err != nil {
				h.serverError(w, fmt.Errorf("update trade journal: %w", err))
				return
			}
		}
	}

	h.renderTradeDetail(w, r, flash)
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
	             a.name, a.firm,
	             je.id, je.tag
	      FROM trades t
	      JOIN accounts a ON a.id = t.account_id
	      LEFT JOIN journal_entries je ON je.trade_id = t.id
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
		var journalID sql.NullInt64
		var journalTag sql.NullString
		if err := rs.Scan(
			&r.ID, &r.AccountID, &r.Symbol, &r.SymbolRoot, &r.Direction, &r.Qty,
			&r.EntryPrice, &r.ExitPrice, &r.EntryTime, &r.ExitTime,
			&r.GrossPnl, &r.Commission, &r.NetPnl, &r.Status,
			&r.AccountName, &r.AccountFirm,
			&journalID, &journalTag,
		); err != nil {
			return nil, err
		}
		if journalID.Valid {
			r.HasJournal = true
		}
		if journalTag.Valid {
			r.JournalTag = journalTag.String
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
	if f.Account.Active {
		if len(f.Account.AccountIDs) == 0 {
			// User requested a filter but no accounts match — return zero rows.
			clauses = append(clauses, "1 = 0")
		} else {
			placeholders := strings.Repeat("?,", len(f.Account.AccountIDs))
			placeholders = placeholders[:len(placeholders)-1]
			clauses = append(clauses, "t.account_id IN ("+placeholders+")")
			for _, id := range f.Account.AccountIDs {
				args = append(args, id)
			}
		}
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
// are treated as "not filtering" — they map to zero values.
//
// Legacy compatibility: `account_id=<n>` (singular) is still honored — it's
// mapped to the new filter=acc:<n> form so old bookmarks and calendar links
// keep working through the transition.
func (h *Handlers) parseTradeFilters(r *http.Request) (tradeFilters, error) {
	q := r.URL.Query()
	f := tradeFilters{Page: 1}

	// Bridge legacy ?account_id=N → ?filter=acc:N before resolving.
	if legacy := strings.TrimSpace(q.Get("account_id")); legacy != "" {
		r2 := *r
		r2.URL = cloneURL(r.URL)
		r2.URL.RawQuery = q.Encode() // start from current
		newQ := r2.URL.Query()
		newQ.Add("filter", "acc:"+legacy)
		r2.URL.RawQuery = newQ.Encode()
		r = &r2
	}

	af, err := h.resolveAccountFilter(r)
	if err != nil {
		return f, err
	}
	f.Account = af

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
	return f, nil
}

// cloneURL returns a shallow copy so we can mutate query params without
// affecting the caller's request URL.
func cloneURL(u *url.URL) *url.URL { c := *u; return &c }

// filtersAsForm flattens the filter struct back into string fields for the
// template (so the simple inputs — symbol/direction/dates — re-render with
// their active values; the multi-select picker reads FilterSelections).
func filtersAsForm(f tradeFilters) map[string]string {
	out := map[string]string{}
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
