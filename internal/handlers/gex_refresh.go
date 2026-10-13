package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Gamma-context refresh (engine update 009). The annotator writes a trade's
// gamma block once and never again (gexMarker is the idempotency key), which
// is right for a stable snapshot history - but the engine now backfills
// outage gaps with reconstructed rows AFTER the fact, and a block written
// during the outage carries the prior session's close, labeled stale. This
// handler re-fetches both moments and replaces the block in place, so an
// entry can pick up the rebuilt rows without losing anything else in its
// body. Only trade and group entries carry gamma blocks.

// gexNoteHeading opens every gamma block buildGexNote writes; replaceGexBlock
// uses it to find where the block starts.
const gexNoteHeading = "_Gamma context (GEX)"

// gexRefreshable reports whether a body's gamma block is one a refresh can
// improve: it was written stale (the moment resolved to a prior session,
// typically an outage) or against a degraded row. A clean measured block
// cannot change - the snapshot history is immutable once captured.
func gexRefreshable(body string) bool {
	mi := strings.Index(body, "<!-- "+gexMarker)
	if mi < 0 {
		return false
	}
	end := strings.Index(body[mi:], "-->")
	if end < 0 {
		return false
	}
	c := body[mi : mi+end]
	return strings.Contains(c, " stale=true") || strings.Contains(c, " quality=degraded")
}

// replaceGexBlock swaps the existing gamma block (heading through the closing
// marker comment) for note. found is false when the body has no complete
// block; changed is false when the new block is byte-identical to the old.
func replaceGexBlock(body, note string) (out string, found, changed bool) {
	mi := strings.Index(body, "<!-- "+gexMarker)
	if mi < 0 {
		return body, false, false
	}
	end := strings.Index(body[mi:], "-->")
	if end < 0 {
		return body, false, false
	}
	end += mi + len("-->")
	start := strings.LastIndex(body[:mi], gexNoteHeading)
	if start < 0 {
		return body, false, false
	}
	note = strings.TrimSpace(note)
	old := body[start:end]
	if old == note {
		return body, true, false
	}
	return body[:start] + note + body[end:], true, true
}

// gexEntryTrade resolves the entry's representative trade the way
// prepare-day did: the earliest-entered closed trade among the entry's own
// trade (trade scope) or its linked trades (group scope). Group entries are
// copy-trader clusters by construction, so every member shares the window.
func (h *Handlers) gexEntryTrade(ctx context.Context, e models.JournalEntry) (prepDayTrade, error) {
	var tradeID int64
	if e.TradeID != nil {
		tradeID = *e.TradeID
	}
	row := h.DB.QueryRowContext(ctx, `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.entry_time, t.entry_price, t.exit_time, t.exit_price, t.account_id, a.name, t.net_pnl
		FROM trades t JOIN accounts a ON a.id = t.account_id
		WHERE a.user_id = ? AND t.status = 'closed' AND t.exit_time IS NOT NULL
		  AND (t.id = ? OR t.id IN (SELECT jet.trade_id FROM journal_entry_trades jet WHERE jet.journal_entry_id = ?))
		ORDER BY t.entry_time ASC, t.id ASC
		LIMIT 1`, userID(ctx), tradeID, e.ID)
	var t prepDayTrade
	err := row.Scan(&t.ID, &t.Symbol, &t.SymbolRoot, &t.Direction, &t.EntryTime, &t.EntryPrice, &t.ExitTime, &t.ExitPrice, &t.AccountID, &t.Account, &t.NetPnl)
	return t, err
}

// refreshEntryGex re-fetches the gamma snapshots for a trade/group entry's
// representative trade and replaces its block. The returned flash is the
// outcome a page shows: gex-refreshed (block rewritten), gex-unchanged (the
// engine still serves the same rows) or gex-failed (no engine, no closed
// trade, an unmapped symbol, or a fetch error - logged). err is reserved
// for storage failures the caller should surface as a server error.
func (h *Handlers) refreshEntryGex(ctx context.Context, e models.JournalEntry) (flash string, err error) {
	if h.GEX == nil {
		return "gex-failed", nil
	}
	t, err := h.gexEntryTrade(ctx, e)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			h.logger().Warn("gex refresh: load trade failed", "entry", e.ID, "err", err)
		}
		return "gex-failed", nil
	}
	market, ok := gexMarket(t.SymbolRoot)
	if !ok || t.ExitPrice == nil {
		return "gex-failed", nil
	}
	enSnap, exSnap, err := h.gexMomentSnapshots(ctx, market, t)
	if err != nil {
		h.logger().Warn("gex refresh: fetch failed", "entry", e.ID, "trade", t.ID, "err", err)
		return "gex-failed", nil
	}
	note := buildGexNote(t, market, enSnap, exSnap, h.gexTickSize(ctx, t.SymbolRoot))
	body, found, changed := replaceGexBlock(e.Body, note)
	switch {
	case !found:
		// No block yet (annotated by an older build, or never): append the
		// way prepare-day would have.
		if err := h.appendOrSeedJournalNote(ctx, e, note); err != nil {
			return "", fmt.Errorf("gex refresh: append: %w", err)
		}
	case !changed:
		return "gex-unchanged", nil
	default:
		if _, err := h.DB.ExecContext(ctx,
			"UPDATE journal_entries SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?",
			body, e.ID, userIDPtr(ctx)); err != nil {
			return "", fmt.Errorf("gex refresh: update: %w", err)
		}
	}
	return "gex-refreshed", nil
}

// JournalGexRefresh handles POST /journal/{id}/gex/refresh for one entry and
// redirects back to its page with the outcome flash.
func (h *Handlers) JournalGexRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: id, UserID: userIDPtr(ctx)})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		h.serverError(w, fmt.Errorf("gex refresh: load entry: %w", err))
		return
	}
	if h.AIReviews.EntryPending(e.ID) {
		// The refresh rewrites the body; the review is appending to it.
		http.Error(w, journal.ErrReviewPending.Error(), http.StatusConflict)
		return
	}
	var back string
	switch e.Scope {
	case "trade":
		if e.TradeID == nil {
			http.Error(w, "trade entry without a trade", http.StatusBadRequest)
			return
		}
		back = fmt.Sprintf("/journal/trade/%d", *e.TradeID)
	case "group":
		back = fmt.Sprintf("/journal/group/%d", e.ID)
	default:
		http.Error(w, "gamma context exists for trade and group entries only", http.StatusBadRequest)
		return
	}
	flash, err := h.refreshEntryGex(ctx, e)
	if err != nil {
		h.serverError(w, err)
		return
	}
	if flash == "gex-refreshed" && h.LiveData != nil {
		h.LiveData.Bump(userID(ctx))
	}
	http.Redirect(w, r, back+"?flash="+flash, http.StatusSeeOther)
}

// gexRepairStart is the first session the engine recomputed in place with
// corrected numerics (update 010, v0.12.0): the default lower bound of the
// bulk refresh, since every gamma block written from those rows before the
// repair carries the old levels.
const gexRepairStart = "2026-09-29"

// gexBulkAbortAfter stops a bulk refresh once this many entries in a row
// failed without a single success: the engine is down, and each further
// attempt would cost a full client timeout.
const gexBulkAbortAfter = 3

// AdminGexRefresh handles POST /admin/market-data/gex-refresh: re-read the
// gamma context of every trade/group entry of the acting user whose entry
// date falls in [from, to] (YYYY-MM-DD, trading-timezone dates; defaults:
// gexRepairStart .. today). Redirects back to the market-data page with the
// counts. Sequential on purpose - one engine, one SQLite writer.
func (h *Handlers) AdminGexRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.GEX == nil {
		http.Redirect(w, r, "/admin/market-data?flash=gexoff", http.StatusSeeOther)
		return
	}
	tz := h.localTZ()
	from, to := strings.TrimSpace(r.FormValue("from")), strings.TrimSpace(r.FormValue("to"))
	if from == "" {
		from = gexRepairStart
	}
	if to == "" {
		to = time.Now().In(tz).Format("2006-01-02")
	}
	for _, d := range []string{from, to} {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			http.Redirect(w, r, "/admin/market-data?flash=gexbad", http.StatusSeeOther)
			return
		}
	}
	if from > to {
		from, to = to, from
	}
	rows, err := h.DB.QueryContext(ctx, `
		SELECT id FROM journal_entries
		WHERE user_id = ? AND scope IN ('trade', 'group') AND instr(body, ?) > 0
		  AND substr(entry_date, 1, 10) BETWEEN ? AND ?
		ORDER BY id`, userID(ctx), "<!-- "+gexMarker, from, to)
	if err != nil {
		h.serverError(w, fmt.Errorf("gex bulk refresh: list entries: %w", err))
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			h.serverError(w, err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()

	var changed, same, failed, streak int
	aborted := false
	for _, id := range ids {
		if h.AIReviews.EntryPending(id) {
			continue // under AI review: its body is being written; the next run catches it
		}
		e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: id, UserID: userIDPtr(ctx)})
		if err != nil {
			failed++
			continue
		}
		flash, err := h.refreshEntryGex(ctx, e)
		if err != nil {
			h.logger().Warn("gex bulk refresh: entry failed", "entry", id, "err", err)
			flash = "gex-failed"
		}
		switch flash {
		case "gex-refreshed":
			changed++
			streak = 0
		case "gex-unchanged":
			same++
			streak = 0
		default:
			failed++
			streak++
		}
		if streak >= gexBulkAbortAfter && changed+same == 0 {
			aborted = true
			break
		}
	}
	if changed > 0 && h.LiveData != nil {
		h.LiveData.Bump(userID(ctx))
	}
	q := url.Values{}
	q.Set("flash", "gexrefresh")
	q.Set("from", from)
	q.Set("to", to)
	q.Set("changed", strconv.Itoa(changed))
	q.Set("same", strconv.Itoa(same))
	q.Set("failed", strconv.Itoa(failed))
	q.Set("total", strconv.Itoa(len(ids)))
	if aborted {
		q.Set("aborted", "1")
	}
	http.Redirect(w, r, "/admin/market-data?"+q.Encode(), http.StatusSeeOther)
}

// gexAdminData is the market-data page's gamma-context card: whether the
// engine is wired, the bulk form's default window, and the last run's
// counts (from the redirect query).
func (h *Handlers) gexAdminData(r *http.Request) map[string]any {
	q := r.URL.Query()
	to := q.Get("to")
	if to == "" {
		to = time.Now().In(h.localTZ()).Format("2006-01-02")
	}
	from := q.Get("from")
	if from == "" {
		from = gexRepairStart
	}
	atoi := func(k string) int { n, _ := strconv.Atoi(q.Get(k)); return n }
	return map[string]any{
		"GexEnabled": h.GEX != nil,
		"GexFrom":    from,
		"GexTo":      to,
		"GexChanged": atoi("changed"),
		"GexSame":    atoi("same"),
		"GexFailed":  atoi("failed"),
		"GexTotal":   atoi("total"),
		"GexAborted": q.Get("aborted") == "1",
	}
}
