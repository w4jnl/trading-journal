package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/excursion"
	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

const tradesPageSize = 50

type tradeFilters struct {
	// Account is the resolved union of acc:N / type:X selections.
	Account    accountFilter
	SymbolRoot *string
	Direction  *string
	// Horizon filters by holding time (derived, not stored): scalp|intraday|swing|long-term.
	Horizon *string
	// Outcome filters by net P&L sign: "win" (net>0), "loss" (net<0),
	// "breakeven" (net=0). nil = no filter. Trades with NULL net_pnl
	// (stub instruments) are excluded by any non-nil outcome filter.
	Outcome *string
	// Risk narrows by the trade's plan: "planned" (carries a stop or risk),
	// "past-stop" (planned, and the measured heat exceeded the risk: the
	// stop was not honoured). nil = no filter.
	Risk *string
	From *time.Time
	To   *time.Time
	// FromDay / ToDay are the trading-timezone calendar days (YYYY-MM-DD)
	// the bounds were built from, for echoing back to the form and chart
	// title. From/To are UTC instants, so formatting them directly shows the
	// previous day for any zone east of UTC.
	FromDay string
	ToDay   string
	// Sort / Dir order the list: Sort is a key of tradeSortColumns ("" =
	// newest entry first), Dir "asc" | "desc".
	Sort string
	Dir  string
	Page int
	// IncludeRetired, when true, lets trades from retired accounts (accounts.active = 0)
	// surface alongside the rest. Default false hides them — matches the
	// retire-account contract: trades stay in the database but only the /trades
	// view can reveal them, behind an opt-in toggle.
	IncludeRetired bool
	// IncludeHidden lifts the Settings account-visibility rule (accounts
	// flagged excluded_from_stats are out of the default list, like on
	// Calendar/Analytics). Only the page's "how many did that hide" shadow
	// count sets it; users override the rule by naming the account in the
	// filter, which buildWhere honors on its own.
	IncludeHidden bool
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
	ID             int64
	AccountID      int64
	Symbol         string
	SymbolRoot     string
	Direction      string
	Qty            float64
	EntryPrice     float64
	ExitPrice      *float64
	EntryTime      time.Time
	ExitTime       *time.Time
	GrossPnl       *float64
	Commission     *float64
	NetPnl         *float64
	PlannedStop    *float64 // the trade's plan (trade_plan.go); nil when none recorded
	PlannedRisk    *float64 // money at risk; R = NetPnl / PlannedRisk
	MaePoints      *float64 // excursion off cached bars (internal/excursion); nil until measured
	MfePoints      *float64
	MaePnl         *float64
	MfePnl         *float64
	ExcursionTf    *string
	Status         string
	AccountName    string
	AccountFirm    *string
	AccountRetired bool // true when account.active = 0; renders a "retired" pill + dimmed row
	HasJournal     bool
	JournalTag     string // "" if no entry or no tag set
	GroupCount     int64  // number of group-journal entries that include this trade
}

// PastStop reports a planned trade whose measured heat exceeded the planned
// risk: price went through the stop and the trade stayed on.
func (r tradeRow) PastStop() bool {
	return r.PlannedRisk != nil && *r.PlannedRisk > 0 && r.MaePnl != nil && *r.MaePnl > *r.PlannedRisk
}

// tradePath is the row glyph of a trade's price path: a bar centred on the
// entry, heat to the left and best-seen to the right (each as a percent of
// the full width, so 50 is the whole half), and the exit as a dot. Each row
// is scaled to its own extreme, so the glyph shows shape, not size.
type tradePath struct {
	HeatPct int
	GainPct int
	ExitPct int // 0..100 across the bar; 50 is the entry
	Title   string
	Sign    string // pnl-pos | pnl-neg | "" for the exit dot
}

// Path is nil until the trade is measured (internal/excursion) and closed.
func (r tradeRow) Path() *tradePath {
	if r.MaePoints == nil || r.MfePoints == nil || r.ExitPrice == nil {
		return nil
	}
	mae, mfe := *r.MaePoints, *r.MfePoints
	scale := math.Max(mae, mfe)
	if scale <= 0 {
		return nil
	}
	realized := excursion.Realized(r.Direction, r.EntryPrice, *r.ExitPrice)
	p := &tradePath{
		HeatPct: int(math.Round(mae / scale * 50)),
		GainPct: int(math.Round(mfe / scale * 50)),
		ExitPct: int(math.Round(50 + realized/scale*50)),
	}
	p.ExitPct = min(max(p.ExitPct, 2), 98)
	switch {
	case realized > 0:
		p.Sign = "pnl-pos"
	case realized < 0:
		p.Sign = "pnl-neg"
	}
	if r.MaePnl != nil && r.MfePnl != nil {
		p.Title = fmt.Sprintf("Heat −%.2f · best seen +%.2f", *r.MaePnl, *r.MfePnl)
	} else {
		p.Title = fmt.Sprintf("Heat %g pts · best seen %g pts", mae, mfe)
	}
	if e := excursion.Efficiency(r.Direction, r.EntryPrice, *r.ExitPrice, mfe); e != nil {
		p.Title += fmt.Sprintf(" · exit kept %.0f%%", *e*100)
	}
	if r.ExcursionTf != nil {
		p.Title += " (" + *r.ExcursionTf + " bars)"
	}
	return p
}

// TradesPage renders /trades — a paginated, filterable table of closed trades.
func (h *Handlers) TradesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	isFragment := liveFragment(r)                // must precede parseTradeFilters/pageRef: strips the flag from the URL
	h.TopstepxSync.MaybeEnqueueUser(userID(ctx)) // throttled background API sync; nil-safe
	h.IbkrSync.MaybeEnqueueUser(userID(ctx))     // throttled IBKR gateway poll; nil-safe
	filters, err := h.parseTradeFilters(r)
	if err != nil {
		h.serverError(w, fmt.Errorf("parse filters: %w", err))
		return
	}

	// If the account filter targets a retired account (e.g., a bookmarked URL
	// from before the account was retired), auto-enable retired visibility so
	// the page isn't empty. Without this the default `account_id IN (active)`
	// clause intersects with `account_id IN (retired)` and returns zero rows.
	retiredAutoOn := false
	if filters.Account.Active && !filters.IncludeRetired {
		retired, rerr := h.retiredAccountSet(ctx)
		if rerr != nil {
			h.serverError(w, fmt.Errorf("retired set: %w", rerr))
			return
		}
		for _, id := range filters.Account.AccountIDs {
			if retired[id] {
				filters.IncludeRetired = true
				retiredAutoOn = true
				break
			}
		}
	}

	rows, err := listTradesFiltered(ctx, h.rdb(), filters)
	if err != nil {
		h.serverError(w, fmt.Errorf("list trades: %w", err))
		return
	}
	totals, err := totalsTradesFiltered(ctx, h.rdb(), filters)
	if err != nil {
		h.serverError(w, fmt.Errorf("count trades: %w", err))
		return
	}
	total := totals.Count

	accounts, err := h.Queries.ListAccounts(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	roots, err := h.Queries.ListDistinctSymbolRoots(ctx, auth.MustUser(ctx).ID)
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
	chartXs, chartYs, chartTitle, chartTotal, err := buildPerfChart(ctx, h.rdb(), h.rq(), filters)
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

	// Existing group-journal entries, surfaced in the bulk-action bar's
	// "Add to existing group" picker. Labels combine date + first words of
	// body so the user can recognize the entry without opening it.
	groupOptions, err := listGroupOptions(ctx, h.DB)
	if err != nil {
		h.serverError(w, fmt.Errorf("list groups: %w", err))
		return
	}

	// Count how many retired-account trades match the rest of the filter so the
	// reveal toggle can advertise "Show N retired" rather than a bare label.
	retiredHidden := int64(0)
	if !filters.IncludeRetired {
		shadow := filters
		shadow.IncludeRetired = true
		shadow.Page = 1
		if all, err := countTradesFiltered(ctx, h.rdb(), shadow); err == nil {
			retiredHidden = all - total
			if retiredHidden < 0 {
				retiredHidden = 0
			}
		}
	}

	// Accounts hidden in Settings (excluded_from_stats) drop out of the
	// default list like they do from Calendar/Analytics; count what the rule
	// hid so the row can say so and point at the filter that overrides it.
	visibilityHidden := int64(0)
	if !filters.Account.Active && !filters.IncludeHidden {
		shadow := filters
		shadow.IncludeHidden = true
		shadow.Page = 1
		if all, err := countTradesFiltered(ctx, h.rdb(), shadow); err == nil && all > total {
			visibilityHidden = all - total
		}
	}

	data := map[string]any{
		"NavActive":        "trades",
		"BackHref":         validBackHref(r),
		"Trades":           rows,
		"Total":            total,
		"Totals":           totals,
		"Sort":             filters.Sort,
		"Dir":              filters.Dir,
		"SortRefs":         sortRefs(r, filters),
		"Page":             filters.Page,
		"Pages":            pages,
		"PageSize":         tradesPageSize,
		"Accounts":         accountsForFilter(accounts, filters.IncludeRetired),
		"SymbolRoots":      roots,
		"Filters":          filtersAsForm(filters),
		"FilterSelections": filters.Account.Selected,
		"FilterCount":      filters.Account.SelectedCount,
		"PrevPage":         pageRef(r, filters.Page-1, pages),
		"NextPage":         pageRef(r, filters.Page+1, pages),
		"ChartXs":          chartXs,       // []int64 Unix timestamps (seconds)
		"ChartYs":          chartYs,       // []float64 cumulative net P&L
		"ChartTitle":       chartTitle,    // empty string when chart is suppressed
		"ChartTotal":       chartTotal,    // final cumulative — drives stroke colour
		"TradeBalances":    tradeBalances, // map[trade_id]float64, missing = no anchor
		"ShowRetired":      filters.IncludeRetired,
		"RetiredAuto":      retiredAutoOn,
		"RetiredHidden":    retiredHidden,
		"VisibilityHidden": visibilityHidden,
		"RetiredQuery":     retiredToggleQuery(r, !filters.IncludeRetired),
		"GroupOptions":     groupOptions,
	}
	if isFragment {
		h.renderPageFragment(w, r, "trades.html", "trades_fragment", data)
		return
	}
	h.renderPage(w, r, "trades.html", data)
}

// groupOption is one row in the "Add to existing group" picker on
// /trades. Label is "YYYY-MM-DD - first-words-of-body" or a fallback
// when the entry has no body yet.
type groupOption struct {
	ID    int64
	Label string
}

// listGroupOptions returns every group journal entry, newest first,
// with a human-readable label for the picker. Done as a hand-rolled
// query because sqlc's typed timestamp scan would require a stricter
// shape than we need here.
func listGroupOptions(ctx context.Context, db *sql.DB) ([]groupOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, entry_date, COALESCE(body, '')
		FROM journal_entries
		WHERE scope = 'group' AND user_id = ?
		ORDER BY entry_date DESC, id DESC
	`, userID(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []groupOption
	for rows.Next() {
		var id int64
		var entryDate time.Time
		var body string
		if err := rows.Scan(&id, &entryDate, &body); err != nil {
			return nil, err
		}
		label := entryDate.Format("2006-01-02")
		// Single line, <=48 runes, machine blocks and markdown stripped
		// (preview.go) so a gamma table never masquerades as the note.
		if excerpt := previewText(body, 47); excerpt != "" {
			label = label + " · " + excerpt
		} else {
			label = label + " · (empty)"
		}
		out = append(out, groupOption{ID: id, Label: label})
	}
	return out, rows.Err()
}

// retiredToggleQuery returns a query string that flips the show_retired flag
// while preserving the rest of the filter state. Used to render the reveal /
// hide toggle's href on /trades. Drops the `page` param so the toggle always
// lands on page 1 (the count under the toggle is the global, not paginated).
func retiredToggleQuery(r *http.Request, on bool) string {
	q := r.URL.Query()
	q.Del("page")
	if on {
		q.Set("show_retired", "1")
	} else {
		q.Del("show_retired")
	}
	enc := q.Encode()
	if enc == "" {
		return ""
	}
	return "?" + enc
}

// buildPerfChart returns the cumulative-P&L curve points for the active
// filters. When from/to aren't set, falls back to the last 60 days from the
// latest closed trade so the chart is always-on (rather than the previous
// behavior of suppressing it on the unfiltered view, which made the surface
// feel inconsistent). Returned arrays are empty only when there are zero
// matching trades within the resolved range.
//
// The cumulative is computed over EVERY filtered trade (not just the visible
// page), ordered by exit_time ASC. The series prepends a (firstExitTime-1min,
// 0) baseline point so the curve starts at zero and ramps to the first trade's
// P&L — matches the visual in exportData/performance.jpg.
func buildPerfChart(ctx context.Context, db *sql.DB, queries *models.Queries, f tradeFilters) (xs []int64, ys []float64, title string, total float64, err error) {
	// "No filter engaged" means no date range AND no account/symbol/direction
	// restriction — in that pristine state we default the chart to the last
	// 60 days from the most recent exit so the page is never empty. The
	// moment the user engages ANY filter, the chart respects whatever
	// buildWhere produces (full span of matching trades).
	anyFilter := f.From != nil || f.To != nil ||
		f.Account.Active || f.SymbolRoot != nil || f.Direction != nil ||
		f.Outcome != nil
	syntheticRange := false
	if !anyFilter {
		// ORDER BY exit_time DESC LIMIT 1 (rather than MAX()) so the driver
		// keeps the column's TIMESTAMP type — modernc.org/sqlite returns
		// MAX() as a string and refuses the implicit conversion back into
		// time.Time. Two sqlc variants because the IN-subquery rule differs
		// between the default view and the show_retired toggle.
		var maxExitPtr *time.Time
		var qerr error
		if f.IncludeRetired {
			maxExitPtr, qerr = queries.MostRecentNetPnLExitTimeIncludingRetired(ctx, userID(ctx))
		} else {
			maxExitPtr, qerr = queries.MostRecentNetPnLExitTime(ctx, userID(ctx))
		}
		if qerr != nil {
			if errors.Is(qerr, sql.ErrNoRows) {
				return nil, nil, "", 0, nil
			}
			return nil, nil, "", 0, qerr
		}
		if maxExitPtr == nil {
			return nil, nil, "", 0, nil
		}
		end := maxExitPtr.Add(24 * time.Hour)
		start := end.Add(-60 * 24 * time.Hour)
		f.From = &start
		f.To = &end
		syntheticRange = true
	}
	where, args := buildWhere(f, userID(ctx))
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

	switch {
	case syntheticRange:
		title = "Last 60 days"
	case f.From != nil && f.To != nil:
		fromStr, toStr := f.fromLabel(), f.toLabel()
		if fromStr == toStr {
			title = "Day Performance · " + fromStr
		} else {
			title = "Range Performance · " + fromStr + " – " + toStr
		}
	case f.From != nil:
		title = "Performance from " + f.fromLabel()
	case f.To != nil:
		title = "Performance through " + f.toLabel()
	default:
		// Filter active but no date — chart covers the full matching span.
		title = "Filtered performance"
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
	trade, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		http.Error(w, "trade not found", http.StatusNotFound)
		return
	}
	// First view of a closed trade measures its MAE/MFE from cached bars
	// (no fetch); the filler covers the rest in the background.
	if h.Excursions != nil && trade.ExcursionTf == nil && trade.ExitTime != nil {
		_, ok, err := h.Excursions.Fill(ctx, excursion.Trade{
			ID: trade.ID, SymbolRoot: trade.SymbolRoot, Direction: trade.Direction, Qty: trade.Qty,
			EntryPrice: trade.EntryPrice, EntryTime: trade.EntryTime, ExitTime: *trade.ExitTime,
		})
		if err != nil {
			h.logger().Warn("excursion on view", "trade", id, "err", err)
		} else if ok {
			if t, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)}); err == nil {
				trade = t
			}
		}
	}
	execs, err := h.Queries.ListExecutionsForTrade(ctx, id)
	if err != nil {
		h.serverError(w, fmt.Errorf("list executions: %w", err))
		return
	}

	// Trade-level journal entry, if one exists. JournalEntryID is passed to
	// the editor so image uploads can be associated with the entry; 0 means
	// "no entry yet" and the attachment endpoint accepts that.
	var journalBody, journalTag string
	var hasJournal bool
	var journalEntryID int64
	var journalStrategyID *int64
	var journalRendered template.HTML
	if entry, err := h.Queries.GetJournalEntryByTradeID(ctx, &id); err == nil {
		hasJournal = true
		journalBody = entry.Body
		journalEntryID = entry.ID
		journalStrategyID = entry.StrategyID
		if entry.Tag != nil {
			journalTag = *entry.Tag
		}
		journalRendered = template.HTML(renderMarkdown(entry.Body))
	} else if !errors.Is(err, sql.ErrNoRows) {
		h.serverError(w, fmt.Errorf("get trade journal: %w", err))
		return
	}

	strategies, err := h.Queries.ListStrategies(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list strategies: %w", err))
		return
	}

	mistakes, emotions := h.entryLabels(ctx, journalEntryID)
	chartTrades := []linkedTrade{{ID: trade.ID, Symbol: trade.Symbol, SymbolRoot: trade.SymbolRoot, Direction: trade.Direction, EntryTime: trade.EntryTime, ExitTime: trade.ExitTime}}
	h.renderPartial(w, "_trade_detail.html", "trade_detail", map[string]any{
		"Trade":            trade,
		"TradeID":          id,
		"Executions":       execs,
		"ChartEnabled":     h.chartShow(chartTrades),
		"ChartEndpoint":    fmt.Sprintf("/charts/trade/%d", id),
		"ChartCached":      h.chartCached(ctx, chartTrades),
		"HasJournal":       hasJournal,
		"JournalBody":      journalBody,
		"JournalTag":       journalTag,
		"Mistakes":         mistakes,
		"Emotions":         emotions,
		"JournalEntryID":   journalEntryID,
		"JournalRendered":  journalRendered,
		"Flash":            flash,
		"Strategies":       strategies,
		"StrategyID":       strategyIDValue(journalStrategyID),
		"AssignedStrategy": h.loadAssignedStrategy(r, journalStrategyID),
	})
}

// TradeJournalSave handles POST /trades/{id}/journal. Empty body deletes
// the entry (matching the day/week save flow). Submissions with the
// HX-Request header return the refreshed trade-detail partial so the
// centered modal on /trades and /analytics updates in place; plain form
// submits from the standalone /journal/trade/{id} page get a 303 redirect
// back to that page with a flash, matching how day/week/group editors
// behave.
func (h *Handlers) TradeJournalSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.guardJournalingDuringAI(w, r) {
		return
	}
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
	strategyID := h.parseStrategyID(r)
	// Explicit Delete button — sets _action=delete so an empty body isn't
	// the only way to remove an entry. Matches the day/week/group flow.
	if r.FormValue("_action") == "delete" {
		body = ""
	}

	mistakes, emotions := labelFields(r)
	out, err := h.journalSvc().SaveTrade(ctx, userID(ctx), tradeID, journal.Fields{Body: body, Tag: tag, StrategyID: strategyID, Mistakes: mistakes, Emotions: emotions})
	if errors.Is(err, journal.ErrNotFound) {
		http.Error(w, "trade not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.saveFailed(w, "save trade journal", err)
		return
	}
	flash := string(out)

	// HTMX path (centered modal on /trades or /analytics) wants the updated
	// detail partial so the modal refreshes in place. A plain form submit
	// from /journal/trade/{id} wants a 303 back to the same page.
	if r.Header.Get("HX-Request") == "true" {
		h.renderTradeDetail(w, r, flash)
		return
	}
	// Stay on the entry page so the just-saved entry shows in preview mode (matching the
	// day/week/group editors); carry `back` as a query param so the page's Back button
	// still returns to wherever the user came from.
	q := url.Values{}
	q.Set("flash", flash)
	if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		q.Set("back", v)
	}
	http.Redirect(w, r, fmt.Sprintf("/journal/trade/%d?%s", tradeID, q.Encode()), http.StatusSeeOther)
}

// listTradesFiltered + countTradesFiltered build a SQL WHERE clause from
// optional filter fields. Each filter contributes a "AND col = ?" clause and
// pushes its value onto the args slice — straightforward, no narg trickery.
func listTradesFiltered(ctx context.Context, db *sql.DB, f tradeFilters) ([]tradeRow, error) {
	where, args := buildWhere(f, userID(ctx))
	args = append(args, tradesPageSize, (f.Page-1)*tradesPageSize)

	q := `SELECT t.id, t.account_id, t.symbol, t.symbol_root, t.direction, t.qty,
	             t.entry_price, t.exit_price, t.entry_time, t.exit_time,
	             t.gross_pnl, t.commission, t.net_pnl, t.planned_stop, t.planned_risk,
	             t.mae_points, t.mfe_points, t.mae_pnl, t.mfe_pnl, t.excursion_tf, t.status,
	             a.name, a.firm, a.active,
	             je.id, je.tag,
	             (SELECT COUNT(*) FROM journal_entry_trades jet WHERE jet.trade_id = t.id) AS group_count
	      FROM trades t
	      JOIN accounts a ON a.id = t.account_id
	      LEFT JOIN journal_entries je ON je.trade_id = t.id
	      WHERE ` + where + `
	      ORDER BY ` + orderBySQL(f) + `
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
		var accountActive int64
		if err := rs.Scan(
			&r.ID, &r.AccountID, &r.Symbol, &r.SymbolRoot, &r.Direction, &r.Qty,
			&r.EntryPrice, &r.ExitPrice, &r.EntryTime, &r.ExitTime,
			&r.GrossPnl, &r.Commission, &r.NetPnl, &r.PlannedStop, &r.PlannedRisk,
			&r.MaePoints, &r.MfePoints, &r.MaePnl, &r.MfePnl, &r.ExcursionTf, &r.Status,
			&r.AccountName, &r.AccountFirm, &accountActive,
			&journalID, &journalTag,
			&r.GroupCount,
		); err != nil {
			return nil, err
		}
		r.AccountRetired = accountActive == 0
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

// tradeSortColumns maps the ?sort= keys the trades table offers to the SQL
// expression they order by. Keys are what the header links carry; the map
// is the whitelist, so nothing user-supplied reaches the query text.
var tradeSortColumns = map[string]string{
	"entry":    "t.entry_time",
	"exit":     "t.exit_time",
	"account":  "a.name",
	"symbol":   "t.symbol",
	"dir":      "t.direction",
	"duration": tradeHoldDaysSQL,
	"qty":      "t.qty",
	"entryp":   "t.entry_price",
	"exitp":    "t.exit_price",
	"gross":    "t.gross_pnl",
	"comm":     "t.commission",
	"net":      "t.net_pnl",
	"r":        "(t.net_pnl / t.planned_risk)", // NULL without a plan -> sorts last
}

// defaultSortDir is the direction a column starts in when first clicked:
// text columns read naturally ascending, numbers and times newest/largest
// first.
func defaultSortDir(col string) string {
	switch col {
	case "account", "symbol", "dir":
		return "asc"
	}
	return "desc"
}

// orderBySQL renders the ORDER BY for the list. NULLS LAST keeps rows
// without the sorted value (a stub instrument's NULL P&L) at the bottom in
// either direction; the id tiebreak makes paging stable.
func orderBySQL(f tradeFilters) string {
	expr, ok := tradeSortColumns[f.Sort]
	if !ok || f.Sort == "" {
		return "t.entry_time DESC, t.id DESC"
	}
	dir := "DESC"
	if f.Dir == "asc" {
		dir = "ASC"
	}
	return expr + " " + dir + " NULLS LAST, t.id DESC"
}

// sortRef builds the header link for col: clicking the active column flips
// its direction, any other column starts in its default direction; the
// page resets to 1 and every filter is kept.
func sortRef(r *http.Request, f tradeFilters, col string) string {
	q := r.URL.Query()
	q.Set("sort", col)
	dir := defaultSortDir(col)
	if f.Sort == col {
		if f.Dir == "desc" {
			dir = "asc"
		} else {
			dir = "desc"
		}
	}
	q.Set("dir", dir)
	q.Del("page")
	return "?" + q.Encode()
}

// sortRefs is the per-column header link map the template indexes.
func sortRefs(r *http.Request, f tradeFilters) map[string]string {
	out := make(map[string]string, len(tradeSortColumns))
	for col := range tradeSortColumns {
		out[col] = sortRef(r, f, col)
	}
	return out
}

// tradeTotals aggregates the WHOLE filtered set (not just the page) for
// the table footer: the number a trader wants after narrowing to a day or
// a setup is the total, which the paged rows cannot show.
type tradeTotals struct {
	Count      int64
	Wins       int64
	Losses     int64
	Gross      float64
	Commission float64
	Net        float64
	// Risk, over the same filtered set. Planned trades carry a risk; RCount
	// of them also have a net P&L (SumR is their R total). PastStop counts
	// planned trades whose measured heat exceeded the risk. Measured trades
	// have an excursion; MeasuredGross / MeasuredMfe is how much of the
	// best move the exits kept.
	Planned       int64
	RCount        int64
	SumR          float64
	PastStop      int64
	Measured      int64
	MeasuredGross float64
	MeasuredMfe   float64
}

// AvgR is the mean R over the planned trades with a P&L; 0 without any.
func (t tradeTotals) AvgR() float64 {
	if t.RCount == 0 {
		return 0
	}
	return t.SumR / float64(t.RCount)
}

// ExitKept is total gross over total best-seen across the measured trades
// (the analytics exit-efficiency figure, for this filter); 0 without MFE.
func (t tradeTotals) ExitKept() float64 {
	if t.MeasuredMfe <= 0 {
		return 0
	}
	return t.MeasuredGross / t.MeasuredMfe
}

func totalsTradesFiltered(ctx context.Context, db *sql.DB, f tradeFilters) (tradeTotals, error) {
	where, args := buildWhere(f, userID(ctx))
	q := `SELECT COUNT(*),
	             COALESCE(SUM(CASE WHEN t.net_pnl > 0 THEN 1 ELSE 0 END), 0),
	             COALESCE(SUM(CASE WHEN t.net_pnl < 0 THEN 1 ELSE 0 END), 0),
	             COALESCE(SUM(t.gross_pnl), 0), COALESCE(SUM(t.commission), 0), COALESCE(SUM(t.net_pnl), 0),
	             COALESCE(SUM(CASE WHEN t.planned_risk > 0 THEN 1 ELSE 0 END), 0),
	             COALESCE(SUM(CASE WHEN t.planned_risk > 0 AND t.net_pnl IS NOT NULL THEN 1 ELSE 0 END), 0),
	             COALESCE(SUM(CASE WHEN t.planned_risk > 0 THEN t.net_pnl / t.planned_risk END), 0),
	             COALESCE(SUM(CASE WHEN t.planned_risk > 0 AND t.mae_pnl > t.planned_risk THEN 1 ELSE 0 END), 0),
	             COALESCE(SUM(CASE WHEN t.mfe_pnl IS NOT NULL THEN 1 ELSE 0 END), 0),
	             COALESCE(SUM(CASE WHEN t.mfe_pnl IS NOT NULL THEN t.gross_pnl END), 0),
	             COALESCE(SUM(t.mfe_pnl), 0)
	      FROM trades t WHERE ` + where
	var tt tradeTotals
	if err := db.QueryRowContext(ctx, q, args...).Scan(&tt.Count, &tt.Wins, &tt.Losses, &tt.Gross, &tt.Commission, &tt.Net,
		&tt.Planned, &tt.RCount, &tt.SumR, &tt.PastStop, &tt.Measured, &tt.MeasuredGross, &tt.MeasuredMfe); err != nil {
		return tradeTotals{}, err
	}
	return tt, nil
}

func countTradesFiltered(ctx context.Context, db *sql.DB, f tradeFilters) (int64, error) {
	where, args := buildWhere(f, userID(ctx))
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
// Retired accounts (accounts.active = 0) are hidden unless f.IncludeRetired
// is true. The caller's user_id is ALWAYS included via a parent-account
// subquery so the user can never see trades on someone else's accounts.
// tradeHoldDaysSQL is a trade's holding time in DAYS. Times are stored as Go strings
// ("2006-01-02 15:04:05.9 -0700 MST"); substr(...,1,19) yields "YYYY-MM-DD HH:MM:SS", the
// datetime form julianday() parses - the full string's " -0700 MST" tail would make it NULL.
// Both entry_time and exit_time are UTC, so the difference is elapsed time. Mirrors
// tradeHorizon()'s bands so the /trades horizon filter matches the per-trade label.
const tradeHoldDaysSQL = "(julianday(substr(t.exit_time,1,19)) - julianday(substr(t.entry_time,1,19)))"

func buildWhere(f tradeFilters, uid int64) (string, []any) {
	w := whereUser(ownsTradesSQL, uid).and("t.status = 'closed'")
	if !f.IncludeRetired {
		w.and("t." + retiredAccountFilterSQL)
	}
	// Settings account visibility: hidden accounts leave the DEFAULT list;
	// an explicit account filter (Active) is the override, same rule as
	// Calendar/Analytics.
	if !f.Account.Active && !f.IncludeHidden {
		w.and("t." + hiddenAccountFilterSQL)
	}
	if f.Account.Active {
		w.andIn("t.account_id", f.Account.AccountIDs)
	}
	if f.SymbolRoot != nil {
		w.and("t.symbol_root = ?", *f.SymbolRoot)
	}
	if f.Direction != nil {
		w.and("t.direction = ?", *f.Direction)
	}
	if f.Horizon != nil {
		// Bands must match tradeHorizon(): Scalp <10m, Intraday <1d, Swing <14d, else Long-term.
		// tradeHoldDaysSQL is the holding time in DAYS.
		switch *f.Horizon {
		case "scalp":
			w.and(tradeHoldDaysSQL+" < ?", 10.0/1440.0)
		case "intraday":
			w.and(tradeHoldDaysSQL+" >= ? AND "+tradeHoldDaysSQL+" < ?", 10.0/1440.0, 1.0)
		case "swing":
			w.and(tradeHoldDaysSQL+" >= ? AND "+tradeHoldDaysSQL+" < ?", 1.0, 14.0)
		case "long-term":
			w.and(tradeHoldDaysSQL+" >= ?", 14.0)
		}
	}
	if f.Outcome != nil {
		switch *f.Outcome {
		case "win":
			w.and("t.net_pnl > 0")
		case "loss":
			w.and("t.net_pnl < 0")
		case "breakeven":
			w.and("t.net_pnl = 0")
		}
	}
	if f.Risk != nil {
		switch *f.Risk {
		case "planned":
			w.and("t.planned_risk > 0")
		case "past-stop":
			w.and("t.planned_risk > 0 AND t.mae_pnl > t.planned_risk")
		}
	}
	if f.From != nil {
		w.and("t.exit_time >= ?", *f.From)
	}
	if f.To != nil {
		w.and("t.exit_time < ?", *f.To)
	}
	return w.sql(), w.args
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

	r = bridgeLegacyAccountID(r) // legacy ?account_id=N -> ?filter=acc:N

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
	if v := strings.TrimSpace(q.Get("horizon")); v == "scalp" || v == "intraday" || v == "swing" || v == "long-term" {
		f.Horizon = &v
	}
	if v := strings.TrimSpace(q.Get("outcome")); v == "win" || v == "loss" || v == "breakeven" {
		f.Outcome = &v
	}
	if v := strings.TrimSpace(q.Get("risk")); v == "planned" || v == "past-stop" {
		f.Risk = &v
	}
	// Dates are trading-tz calendar days (keywords today|yesterday|last are
	// resolved server-side), converted to UTC instants: exit_time is stored
	// in UTC and compared as text, so a Chicago day must be expressed as its
	// UTC bounds or the evening hours land on the wrong day. Junk is ignored.
	tz := h.localTZ()
	if v := strings.TrimSpace(q.Get("from")); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", h.resolveDayKeyword(r.Context(), v, tz), tz); err == nil {
			u := t.UTC()
			f.From = &u
			f.FromDay = t.Format("2006-01-02")
		}
	}
	if v := strings.TrimSpace(q.Get("to")); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", h.resolveDayKeyword(r.Context(), v, tz), tz); err == nil {
			u := t.AddDate(0, 0, 1).UTC() // inclusive upper bound
			f.To = &u
			f.ToDay = t.Format("2006-01-02")
		}
	}
	if v := strings.TrimSpace(q.Get("sort")); v != "" {
		if _, ok := tradeSortColumns[v]; ok {
			f.Sort = v
			f.Dir = defaultSortDir(v)
		}
	}
	if v := strings.TrimSpace(q.Get("dir")); f.Sort != "" && (v == "asc" || v == "desc") {
		f.Dir = v
	}
	if v := strings.TrimSpace(q.Get("page")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p >= 1 {
			f.Page = p
		}
	}
	if v := strings.TrimSpace(q.Get("show_retired")); v == "1" || v == "on" || v == "true" {
		f.IncludeRetired = true
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
	if f.Horizon != nil {
		out["Horizon"] = *f.Horizon
	}
	if f.Outcome != nil {
		out["Outcome"] = *f.Outcome
	}
	if f.Risk != nil {
		out["Risk"] = *f.Risk
	}
	if f.From != nil {
		out["From"] = f.fromLabel()
	}
	if f.To != nil {
		out["To"] = f.toLabel()
	}
	return out
}

// fromLabel / toLabel are the user-facing days of the range. The parser
// records them (FromDay/ToDay); a filter built elsewhere falls back to the
// UTC instant, which is right only for zones at or west of UTC.
func (f tradeFilters) fromLabel() string {
	if f.FromDay != "" {
		return f.FromDay
	}
	return f.From.Format("2006-01-02")
}

func (f tradeFilters) toLabel() string {
	if f.ToDay != "" {
		return f.ToDay
	}
	return f.To.Add(-24 * time.Hour).Format("2006-01-02") // To is exclusive +24h; restore the user-facing day
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
