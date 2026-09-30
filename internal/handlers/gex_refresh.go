package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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

// JournalGexRefresh handles POST /journal/{id}/gex/refresh: re-fetch the
// gamma snapshots for the entry's representative trade and replace the
// block. Redirects back to the entry's page with a flash: gex-refreshed,
// gex-unchanged (the engine still serves the same rows) or gex-failed (no
// engine, no closed trade, an unmapped symbol, or a fetch error - logged).
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
	redirect := func(flash string) {
		http.Redirect(w, r, back+"?flash="+flash, http.StatusSeeOther)
	}
	if h.GEX == nil {
		redirect("gex-failed")
		return
	}
	t, err := h.gexEntryTrade(ctx, e)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			h.logger().Warn("gex refresh: load trade failed", "entry", id, "err", err)
		}
		redirect("gex-failed")
		return
	}
	market, ok := gexMarket(t.SymbolRoot)
	if !ok || t.ExitPrice == nil {
		redirect("gex-failed")
		return
	}
	enSnap, exSnap, err := h.gexMomentSnapshots(ctx, market, t)
	if err != nil {
		h.logger().Warn("gex refresh: fetch failed", "entry", id, "trade", t.ID, "err", err)
		redirect("gex-failed")
		return
	}
	note := buildGexNote(t, market, enSnap, exSnap, h.gexTickSize(ctx, t.SymbolRoot))
	body, found, changed := replaceGexBlock(e.Body, note)
	switch {
	case !found:
		// No block yet (annotated by an older build, or never): append the
		// way prepare-day would have.
		if err := h.appendOrSeedJournalNote(ctx, e, note); err != nil {
			h.serverError(w, fmt.Errorf("gex refresh: append: %w", err))
			return
		}
	case !changed:
		redirect("gex-unchanged")
		return
	default:
		if _, err := h.DB.ExecContext(ctx,
			"UPDATE journal_entries SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?",
			body, e.ID, userIDPtr(ctx)); err != nil {
			h.serverError(w, fmt.Errorf("gex refresh: update: %w", err))
			return
		}
	}
	if h.LiveData != nil {
		h.LiveData.Bump(userID(ctx))
	}
	redirect("gex-refreshed")
}
