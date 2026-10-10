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

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// plural is a tiny helper for "1 trade" vs "2 trades" pluralization in
// export text. Returns "s" when n != 1.
func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// signedMoney formats a P&L value with an explicit sign — used by the
// markdown and PDF group renderers so "+$42.50" / "-$13.00" reads cleanly.
// formatQty renders a trade quantity without trailing zeros — futures
// quantities are integer in our world, so "%.0f" reads better than "%g"
// or the full float form.
func formatQty(q float64) string {
	if q == float64(int64(q)) {
		return fmt.Sprintf("%d", int64(q))
	}
	return fmt.Sprintf("%.2f", q)
}

func signedMoney(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("+$%.2f", v)
}

// GroupSummary aggregates the trades linked to one group journal entry.
// NetPnl-NULL trades count toward N but contribute 0 to the money totals —
// same convention as elsewhere when a stub instrument hasn't been configured.
type GroupSummary struct {
	N          int64
	Net        float64
	Gross      float64
	Commission float64
	Wins       int64
	Losses     int64
	LastExit   time.Time
}

// linkedTrade is the per-row projection used by the group-entry editor's
// linked-trade table. Day/week journal views also reuse this shape to
// render their own trades-in-scope lists, so it carries gross/commission
// even though the table itself only displays net.
// Exported fields so html/template can read them.
type linkedTrade struct {
	ID          int64
	Symbol      string
	SymbolRoot  string
	Direction   string
	Qty         float64
	AccountName string
	EntryTime   time.Time
	ExitTime    *time.Time
	GrossPnl    *float64
	Commission  *float64
	NetPnl      *float64
}

// summarizeTradeRows folds a slice of linkedTrade into the same shape as
// summarizeGroupTrades — wins/losses/net/gross/commission — so day, week,
// and group views can render an identical summary card from any source
// of trades.
func summarizeTradeRows(trades []linkedTrade) GroupSummary {
	var s GroupSummary
	for _, t := range trades {
		s.N++
		if t.GrossPnl != nil {
			s.Gross += *t.GrossPnl
		}
		if t.Commission != nil {
			s.Commission += *t.Commission
		}
		if t.NetPnl != nil {
			s.Net += *t.NetPnl
			switch {
			case *t.NetPnl > 0:
				s.Wins++
			case *t.NetPnl < 0:
				s.Losses++
			}
		}
		if t.ExitTime != nil && t.ExitTime.After(s.LastExit) {
			s.LastExit = *t.ExitTime
		}
	}
	return s
}

// loadDayTrades returns all closed trades exiting on the given local
// trading day, ordered by exit_time ASC. Used by the day journal view so
// the page shows the same "context-trades" panel as the group view.
//
// Retired accounts (accounts.active = 0) are excluded - the day summary
// matches the calendar / analytics / dashboard convention of hiding
// retired-account trades from stats. The /trades page retains its own
// reveal toggle for explicit inspection.
func loadDayTrades(ctx context.Context, db *sql.DB, date time.Time) ([]linkedTrade, error) {
	// date is local midnight; SQL compares UTC text (see daybounds.go).
	fromU, toU := dayBoundsUTC(date)
	return scanLinkedTrades(ctx, db, `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.qty, a.name,
		       t.entry_time, t.exit_time, t.gross_pnl, t.commission, t.net_pnl
		FROM trades t
		JOIN accounts a ON a.id = t.account_id
		WHERE t.exit_time IS NOT NULL
		  AND a.active = 1
		  AND a.user_id = ?
		  AND t.exit_time >= ? AND t.exit_time < ?
		ORDER BY t.exit_time ASC, t.id ASC
	`, userID(ctx), fromU, toU)
}

// loadOneTrade returns a one-element slice for the given trade id, in the
// shared linkedTrade shape. Used by the exporters so a trade-scope entry
// can list its (single) underlying trade with the same formatting as
// day/week/group entries.
func loadOneTrade(ctx context.Context, db *sql.DB, tradeID int64) ([]linkedTrade, error) {
	return scanLinkedTrades(ctx, db, `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.qty, a.name,
		       t.entry_time, t.exit_time, t.gross_pnl, t.commission, t.net_pnl
		FROM trades t
		JOIN accounts a ON a.id = t.account_id
		WHERE t.id = ? AND a.user_id = ?
	`, tradeID, userID(ctx))
}

// loadWeekTrades returns all closed trades exiting within the ISO week that
// starts at `monday` (Monday..Sunday inclusive). Excludes retired-account
// trades for the same reason loadDayTrades does.
func loadWeekTrades(ctx context.Context, db *sql.DB, monday time.Time) ([]linkedTrade, error) {
	fromU, toU := daysBoundsUTC(monday, 7)
	return scanLinkedTrades(ctx, db, `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.qty, a.name,
		       t.entry_time, t.exit_time, t.gross_pnl, t.commission, t.net_pnl
		FROM trades t
		JOIN accounts a ON a.id = t.account_id
		WHERE t.exit_time IS NOT NULL
		  AND a.active = 1
		  AND a.user_id = ?
		  AND t.exit_time >= ? AND t.exit_time < ?
		ORDER BY t.exit_time ASC, t.id ASC
	`, userID(ctx), fromU, toU)
}

// scanLinkedTrades is the shared scan path for loadDayTrades / loadWeekTrades
// / loadLinkedTrades. Same column order in all three queries.
func scanLinkedTrades(ctx context.Context, db *sql.DB, query string, args ...any) ([]linkedTrade, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []linkedTrade
	for rows.Next() {
		var lt linkedTrade
		var exit sql.NullTime
		if err := rows.Scan(&lt.ID, &lt.Symbol, &lt.SymbolRoot, &lt.Direction, &lt.Qty,
			&lt.AccountName, &lt.EntryTime, &exit,
			&lt.GrossPnl, &lt.Commission, &lt.NetPnl); err != nil {
			return nil, err
		}
		if exit.Valid {
			t := exit.Time
			lt.ExitTime = &t
		}
		out = append(out, lt)
	}
	return out, rows.Err()
}

// summarizeGroupTrades aggregates trades linked to a single group entry.
// Returns a zero-value GroupSummary when the entry has no linked trades.
func summarizeGroupTrades(ctx context.Context, db *sql.DB, journalEntryID int64) (GroupSummary, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.gross_pnl, t.commission, t.net_pnl, t.exit_time
		FROM journal_entry_trades jet
		JOIN trades t ON t.id = jet.trade_id
		WHERE jet.journal_entry_id = ?
	`, journalEntryID)
	if err != nil {
		return GroupSummary{}, fmt.Errorf("summarize group: %w", err)
	}
	defer rows.Close()
	var s GroupSummary
	for rows.Next() {
		var gross, comm, net *float64
		var exit sql.NullTime
		if err := rows.Scan(&gross, &comm, &net, &exit); err != nil {
			return GroupSummary{}, err
		}
		s.N++
		if gross != nil {
			s.Gross += *gross
		}
		if comm != nil {
			s.Commission += *comm
		}
		if net != nil {
			s.Net += *net
			switch {
			case *net > 0:
				s.Wins++
			case *net < 0:
				s.Losses++
			}
		}
		if exit.Valid && exit.Time.After(s.LastExit) {
			s.LastExit = exit.Time
		}
	}
	return s, rows.Err()
}

// loadGroupSummaries batches summaries for many group entries. Returns a map
// keyed by journal_entry_id; entries with no linked trades are absent (the
// template can fall back to a zero-value default via map lookup).
func loadGroupSummaries(ctx context.Context, db *sql.DB, entryIDs []int64) (map[int64]GroupSummary, error) {
	out := map[int64]GroupSummary{}
	if len(entryIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(entryIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(entryIDs))
	for _, id := range entryIDs {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT jet.journal_entry_id,
		       t.gross_pnl, t.commission, t.net_pnl, t.exit_time
		FROM journal_entry_trades jet
		JOIN trades t ON t.id = jet.trade_id
		WHERE jet.journal_entry_id IN (`+placeholders+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("batch summarize: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eid int64
		var gross, comm, net *float64
		var exit sql.NullTime
		if err := rows.Scan(&eid, &gross, &comm, &net, &exit); err != nil {
			return nil, err
		}
		s := out[eid]
		s.N++
		if gross != nil {
			s.Gross += *gross
		}
		if comm != nil {
			s.Commission += *comm
		}
		if net != nil {
			s.Net += *net
			switch {
			case *net > 0:
				s.Wins++
			case *net < 0:
				s.Losses++
			}
		}
		if exit.Valid && exit.Time.After(s.LastExit) {
			s.LastExit = exit.Time
		}
		out[eid] = s
	}
	return out, rows.Err()
}

// loadLinkedTrades returns the trades linked to a group entry, ordered by
// exit_time ASC. Uses the shared scanLinkedTrades path so day, week, and
// group views all hydrate the same struct shape.
func loadLinkedTrades(ctx context.Context, db *sql.DB, journalEntryID int64) ([]linkedTrade, error) {
	return scanLinkedTrades(ctx, db, `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.qty, a.name,
		       t.entry_time, t.exit_time, t.gross_pnl, t.commission, t.net_pnl
		FROM journal_entry_trades jet
		JOIN trades t   ON t.id = jet.trade_id
		JOIN accounts a ON a.id = t.account_id
		WHERE jet.journal_entry_id = ?
		ORDER BY t.exit_time ASC, t.id ASC
	`, journalEntryID)
}

// GroupJournalCreate handles POST /journal/group/new — creates a new
// scope='group' entry, links the selected trades, and redirects to the
// editor. Form fields:
//
//	trade_id  (one or many, all values are parsed; non-numerics ignored)
//	back      (optional /-rooted URL to round-trip via the editor save)
func (h *Handlers) GroupJournalCreate(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAI(w, r) {
		return
	}
	ctx := r.Context()
	tz := h.localTZ()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	raw := r.Form["trade_id"]
	ids := make([]int64, 0, len(raw))
	for _, s := range raw {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Error(w, "select at least one trade", http.StatusBadRequest)
		return
	}

	// entry_date = max(exit_time) across the selected trades in trading tz.
	// ORDER BY ... LIMIT 1 (not MAX(exit_time)) keeps the scan path on a
	// typed column — same workaround as pickDashboardDate.
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	// Scope the trade lookup by user so the "find latest exit" query can't
	// surface a trade the user doesn't own (the trade id came from the form).
	scopedArgs := append([]any{userID(ctx)}, args...)
	var maxExit time.Time
	row := h.DB.QueryRowContext(ctx,
		"SELECT t.exit_time FROM trades t JOIN accounts a ON a.id = t.account_id WHERE a.user_id = ? AND t.id IN ("+placeholders+") AND t.exit_time IS NOT NULL ORDER BY t.exit_time DESC LIMIT 1",
		scopedArgs...,
	)
	if err := row.Scan(&maxExit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			maxExit = time.Now().In(tz)
		} else {
			h.serverError(w, fmt.Errorf("find max exit: %w", err))
			return
		}
	}
	y, m, d := maxExit.In(tz).Date()
	entryDate := time.Date(y, m, d, 0, 0, 0, 0, tz)

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		h.serverError(w, fmt.Errorf("begin: %w", err))
		return
	}
	defer tx.Rollback()
	q := h.Queries.WithTx(tx)
	entry, err := q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID:    userIDPtr(ctx),
		TradeID:   nil,
		AccountID: nil,
		EntryDate: entryDate,
		Body:      "",
		Mood:      nil,
		Tag:       nil,
		Scope:     "group",
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("create group entry: %w", err))
		return
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)",
			entry.ID, id,
		); err != nil {
			h.serverError(w, fmt.Errorf("link trade %d: %w", id, err))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.serverError(w, fmt.Errorf("commit: %w", err))
		return
	}

	target := fmt.Sprintf("/journal/group/%d", entry.ID)
	h.LiveData.Bump(userID(ctx))

	if back := r.FormValue("back"); auth.SafeLocalPath(back) {
		v := url.Values{}
		v.Set("back", back)
		target += "?" + v.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// GroupJournalPage renders /journal/group/{id} — the editor for one group
// entry with its linked trades, P&L summary, body, and tag.
func (h *Handlers) GroupJournalPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var entry models.JournalEntry
	row := h.DB.QueryRowContext(ctx, `
		SELECT id, trade_id, account_id, entry_date, body, mood, tag, scope, created_at, updated_at, strategy_id
		FROM journal_entries WHERE id = ? AND scope = 'group' AND user_id = ?
	`, id, userID(ctx))
	if err := row.Scan(&entry.ID, &entry.TradeID, &entry.AccountID, &entry.EntryDate,
		&entry.Body, &entry.Mood, &entry.Tag, &entry.Scope, &entry.CreatedAt, &entry.UpdatedAt, &entry.StrategyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		h.serverError(w, fmt.Errorf("get group entry: %w", err))
		return
	}

	trades, err := loadLinkedTrades(ctx, h.rdb(), id)
	if err != nil {
		h.serverError(w, err)
		return
	}
	// Compute the summary in Go from the already-loaded trades so day,
	// week, and group views all run through the same summarizeTradeRows
	// path (one source of truth for Net / Gross / Comm / W-L).
	summary := summarizeTradeRows(trades)

	var rendered template.HTML
	if entry.Body != "" {
		rendered = template.HTML(renderMarkdown(entry.Body))
	}
	tag := ""
	if entry.Tag != nil {
		tag = *entry.Tag
	}

	strategies, err := h.Queries.ListStrategies(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list strategies: %w", err))
		return
	}

	// Candidate trades for the "+ Add trades" picker: the user's recent
	// closed trades not already linked to this group.
	candidates, err := loadLinkableTrades(ctx, h.rdb(), userID(ctx), id, 300)
	if err != nil {
		h.serverError(w, fmt.Errorf("load linkable trades: %w", err))
		return
	}
	picks := make([]groupTradePick, 0, len(candidates))
	for _, t := range candidates {
		entryUTC := t.EntryTime.UTC().Format("2006-01-02 15:04")
		picks = append(picks, groupTradePick{
			ID:         t.ID,
			EntryTime:  t.EntryTime,
			Symbol:     t.Symbol,
			SymbolRoot: t.SymbolRoot,
			Direction:  t.Direction,
			Qty:        t.Qty,
			NetPnl:     t.NetPnl,
			Account:    t.AccountName,
			Search:     strings.ToLower(entryUTC + " " + t.Symbol + " " + t.SymbolRoot + " " + t.Direction + " " + t.AccountName),
		})
	}

	h.renderPage(w, r, "journal_group.html", map[string]any{
		"NavActive":        "journal",
		"BackHref":         validBackHref(r),
		"EntryID":          id,
		"ChartEnabled":     h.chartShow(trades),
		"ChartEndpoint":    fmt.Sprintf("/charts/group/%d", id),
		"ChartCached":      h.chartCached(ctx, trades),
		"Entry":            entry,
		"GexRefresh":       h.GEX != nil && gexRefreshable(entry.Body),
		"EntryDate":        entry.EntryDate.In(tz).Format("2006-01-02"),
		"EntryDateNice":    entry.EntryDate.In(tz).Format("Mon, 2 Jan 2006"),
		"Trades":           trades,
		"Summary":          summary,
		"Tag":              tag,
		"Rendered":         rendered,
		"Flash":            r.URL.Query().Get("flash"),
		"Strategies":       strategies,
		"StrategyID":       strategyIDValue(entry.StrategyID),
		"AssignedStrategy": h.loadAssignedStrategy(r, entry.StrategyID),
		"CandidateTrades":  picks,
	})
}

// groupTradePick is one row in the group editor's "+ Add trades" picker.
// Date is pre-formatted in the trading tz and Search is a lowercased blob
// the client filter matches against, so the template stays tz-agnostic.
type groupTradePick struct {
	ID         int64
	EntryTime  time.Time // rendered browser-local via the lt helper
	Symbol     string
	SymbolRoot string
	Direction  string
	Qty        float64
	NetPnl     *float64
	Account    string
	Search     string
}

// loadLinkableTrades returns the user's recent closed trades NOT already
// linked to entryID, newest first, capped at limit.
func loadLinkableTrades(ctx context.Context, db *sql.DB, userID, entryID int64, limit int) ([]linkedTrade, error) {
	return scanLinkedTrades(ctx, db, `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.qty, a.name,
		       t.entry_time, t.exit_time, t.gross_pnl, t.commission, t.net_pnl
		FROM trades t
		JOIN accounts a ON a.id = t.account_id
		WHERE a.user_id = ?
		  AND t.status = 'closed' AND t.exit_time IS NOT NULL
		  AND t.id NOT IN (SELECT trade_id FROM journal_entry_trades WHERE journal_entry_id = ?)
		ORDER BY t.exit_time DESC, t.id DESC
		LIMIT ?`, userID, entryID, limit)
}

// GroupJournalSave handles POST /journal/group/{id}. Empty body deletes the
// entry (its junction rows go via ON DELETE CASCADE).
func (h *Handlers) GroupJournalSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.guardJournalingDuringAI(w, r) {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	redirectTo := func(flash string) string {
		q := url.Values{}
		q.Set("flash", flash)
		if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
			q.Set("back", v)
		}
		if flash == "deleted" {
			return "/journal?" + q.Encode()
		}
		return fmt.Sprintf("/journal/group/%d?%s", id, q.Encode())
	}
	body := strings.TrimSpace(r.FormValue("body"))
	tag := parseTag(r.FormValue("tag"))
	strategyID := h.parseStrategyID(r)
	if body == "" {
		if err := h.Queries.DeleteJournalEntry(ctx, models.DeleteJournalEntryParams{ID: id, UserID: userIDPtr(ctx)}); err != nil {
			h.serverError(w, fmt.Errorf("delete group: %w", err))
			return
		}
		h.journalChanged(ctx)
		http.Redirect(w, r, redirectTo("deleted"), http.StatusSeeOther)
		return
	}
	if err := h.Queries.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
		UserID:     userIDPtr(ctx),
		Body:       body,
		Mood:       nil,
		Tag:        tag,
		StrategyID: strategyID,
		ID:         id,
	}); err != nil {
		h.serverError(w, fmt.Errorf("update group: %w", err))
		return
	}
	h.journalChanged(ctx)
	http.Redirect(w, r, redirectTo("saved"), http.StatusSeeOther)
}

// GroupJournalDelete handles POST /journal/group/{id}/delete — explicit
// delete from the editor's Delete button. ON DELETE CASCADE on the junction
// table removes linked-trade rows. Redirects to the feed with flash=deleted.
func (h *Handlers) GroupJournalDelete(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAI(w, r) {
		return
	}
	ctx := r.Context()
	entryID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if err := h.Queries.DeleteJournalEntry(ctx, models.DeleteJournalEntryParams{ID: entryID, UserID: userIDPtr(ctx)}); err != nil {
		h.serverError(w, fmt.Errorf("delete group: %w", err))
		return
	}
	h.journalChanged(ctx)
	q := url.Values{}
	q.Set("flash", "deleted")
	target := "/journal"
	if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		target = v
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + q.Encode()
	} else {
		target += "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// GroupJournalLink handles POST /journal/group/{id}/link — adds one or
// more trades to the group. Accepts one or many trade_id form values
// (single-trade form on the group editor, bulk action bar on /trades).
// Idempotent via INSERT OR IGNORE on the composite PK.
//
// An optional `back` form field (must be a /-rooted relative URL)
// redirects the caller back to where they came from with a
// `?flash=grouplinked=N&group=ID` annotation, so the /trades bulk
// flow can stay in place rather than navigating into the editor.
func (h *Handlers) GroupJournalLink(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAI(w, r) {
		return
	}
	ctx := r.Context()
	entryID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	raw := r.Form["trade_id"]
	ids := make([]int64, 0, len(raw))
	for _, s := range raw {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Error(w, "no trade_id", http.StatusBadRequest)
		return
	}

	// Ownership: the group entry must belong to the caller.
	var ownEntry int64
	if err := h.DB.QueryRowContext(ctx,
		"SELECT 1 FROM journal_entries WHERE id = ? AND user_id = ? AND scope = 'group'",
		entryID, userID(ctx),
	).Scan(&ownEntry); err != nil {
		http.NotFound(w, r)
		return
	}
	// Keep only trade ids the caller actually owns, so a guessed/foreign id
	// can't be linked into the user's group entry.
	owned := make([]int64, 0, len(ids))
	for _, id := range ids {
		var ok int64
		if err := h.DB.QueryRowContext(ctx,
			"SELECT 1 FROM trades t JOIN accounts a ON a.id = t.account_id WHERE t.id = ? AND a.user_id = ?",
			id, userID(ctx),
		).Scan(&ok); err == nil {
			owned = append(owned, id)
		}
	}
	if len(owned) == 0 {
		http.Error(w, "no owned trades to link", http.StatusBadRequest)
		return
	}
	ids = owned

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		h.serverError(w, fmt.Errorf("begin: %w", err))
		return
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)",
			entryID, id,
		); err != nil {
			h.serverError(w, fmt.Errorf("link trade %d: %w", id, err))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.serverError(w, fmt.Errorf("commit: %w", err))
		return
	}
	h.journalChanged(ctx)

	if back := r.FormValue("back"); back != "" && strings.HasPrefix(back, "/") && !strings.HasPrefix(back, "//") {
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		http.Redirect(w, r,
			fmt.Sprintf("%s%sflash=grouplinked%%3D%d&group=%d", back, sep, len(ids), entryID),
			http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/journal/group/%d?flash=linked", entryID), http.StatusSeeOther)
}

// GroupJournalUnlink handles POST /journal/group/{id}/unlink — removes a
// trade from the group.
func (h *Handlers) GroupJournalUnlink(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAI(w, r) {
		return
	}
	ctx := r.Context()
	entryID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	tradeID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("trade_id")), 10, 64)
	if err != nil {
		http.Error(w, "bad trade_id", http.StatusBadRequest)
		return
	}
	// Ownership lives in the predicate: the link is removed only when the
	// group entry is the caller's. Anything else is a 404, like the link
	// handler's guard above - a foreign id must not confirm or mutate.
	res, err := h.DB.ExecContext(ctx,
		`DELETE FROM journal_entry_trades
		 WHERE journal_entry_id = ? AND trade_id = ?
		   AND journal_entry_id IN (SELECT id FROM journal_entries WHERE user_id = ? AND scope = 'group')`,
		entryID, tradeID, userID(ctx),
	)
	if err != nil {
		h.serverError(w, fmt.Errorf("unlink: %w", err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.NotFound(w, r)
		return
	}
	h.LiveData.Bump(userID(ctx))
	http.Redirect(w, r, fmt.Sprintf("/journal/group/%d?flash=unlinked", entryID), http.StatusSeeOther)
}
