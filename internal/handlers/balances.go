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

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// BalanceAnchor is a user-stated balance at a specific date. Running balance
// at any later date = anchor.Balance + sum(net_pnl) over trades exited after
// the anchor up to that date.
type BalanceAnchor struct {
	ID         int64
	AccountID  int64
	AnchorDate time.Time
	Balance    float64
	Note       string
}

// AccountBalance is the computed running balance for an account, returned to
// the template. Known is false when the account has no anchors yet.
type AccountBalance struct {
	Known       bool
	Balance     float64
	AsOf        time.Time     // the "today" date the balance was computed for
	LatestDate  time.Time     // the latest anchor's anchor_date that drove the result
	HasNoChange bool          // true when no closed trades have exited since the anchor
	Anchors     []BalanceAnchor
}

// loadBalances fetches every anchor across all accounts plus every closed
// trade's (account_id, exit_date, net_pnl). It then walks the data once to
// produce a current running balance per account. Done in one pass so the
// /accounts page stays a single query batch.
func (h *Handlers) loadBalances(ctx context.Context, today time.Time) (map[int64]AccountBalance, error) {
	tz := h.localTZ()

	anchorRows, err := h.Queries.ListBalanceAnchors(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		return nil, fmt.Errorf("list balance_anchors: %w", err)
	}
	byAccount := map[int64][]BalanceAnchor{}
	for _, r := range anchorRows {
		a := BalanceAnchor{
			ID:         r.ID,
			AccountID:  r.AccountID,
			AnchorDate: r.AnchorDate,
			Balance:    r.Balance,
			Note:       r.Note,
		}
		byAccount[a.AccountID] = append(byAccount[a.AccountID], a)
	}

	type tradeDelta struct {
		ExitTime time.Time // raw UTC timestamp; compared directly so the result is tz-independent
		NetPnL   float64
	}
	tradeRows, err := h.Queries.ListClosedTradeDeltasOrdered(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		return nil, fmt.Errorf("list trades: %w", err)
	}
	// Bucket deltas by account so the per-account loop below is O(n) in that
	// account's deltas, not in all deltas across every account.
	deltasByAccount := map[int64][]tradeDelta{}
	for _, r := range tradeRows {
		if r.ExitTime == nil {
			continue
		}
		deltasByAccount[r.AccountID] = append(deltasByAccount[r.AccountID], tradeDelta{
			ExitTime: *r.ExitTime,
			NetPnL:   r.NetPnl,
		})
	}

	// We pick "most recent anchor at or before now" without rounding to a
	// calendar day. anchor_date was originally stored as midnight in the
	// creator's tz, so a viewer in a different tz would round it to a
	// different day and silently skip otherwise-valid trades on the
	// boundary.
	out := map[int64]AccountBalance{}
	for accountID, anchors := range byAccount {
		var anchor BalanceAnchor
		forward := true
		found := false
		for _, a := range anchors {
			if !a.AnchorDate.After(today) {
				anchor = a
				found = true
			}
		}
		if !found {
			anchor = anchors[0]
			forward = false
		}

		var delta float64
		for _, td := range deltasByAccount[accountID] {
			if forward {
				// Sum every closed trade whose exit happened after the
				// anchor's stored moment. Timestamp comparison only - no
				// dayStart() round-trip, so the result is the same whether
				// the viewer's tz is Chicago, Amsterdam, or UTC.
				if td.ExitTime.After(anchor.AnchorDate) {
					delta += td.NetPnL
				}
			} else {
				// Anchor is in the future. Subtract trades whose exit time
				// falls between now and the anchor's stored moment - this
				// branch only matters if such trades exist (system clock
				// vs stored exit_time), so in normal data it's a no-op.
				if td.ExitTime.After(today) && !td.ExitTime.After(anchor.AnchorDate) {
					delta -= td.NetPnL
				}
			}
		}

		out[accountID] = AccountBalance{
			Known:       true,
			Balance:     anchor.Balance + delta,
			AsOf:        dayStart(today, tz),
			LatestDate:  dayStart(anchor.AnchorDate, tz),
			HasNoChange: delta == 0,
			Anchors:     anchors,
		}
	}
	return out, nil
}

func dayStart(t time.Time, tz *time.Location) time.Time {
	y, m, d := t.In(tz).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, tz)
}

// loadTradeBalances returns map[trade_id] → balance-after-this-trade for every
// closed trade on an account that has at least one balance anchor. Trades on
// anchorless accounts are absent from the map; the template renders "—" for
// those rows.
//
// Algorithm — for each account independently:
//   1. Sort the account's closed trades by exit_time ASC.
//   2. Sort its anchors by anchor_date ASC. The anchor balance is taken to
//      mean "end-of-day on anchor_date" (every trade exited on or before
//      that day's last instant is already baked into it).
//   3. For each trade, pick the relevant anchor: the latest anchor whose date
//      is ≤ the trade's exit-day, or — if all anchors are in the future —
//      the earliest anchor (back-fill).
//   4. Compute balance_after via a cumulative net-P&L array:
//      - trade at index i ≤ anchorEndIdx  (trade is in the anchor's past):
//          balance = anchor.Balance − Σ net_pnl(j) for j in (i, anchorEndIdx]
//      - trade at index i > anchorEndIdx  (trade is in the anchor's future):
//          balance = anchor.Balance + Σ net_pnl(j) for j in (anchorEndIdx, i]
func (h *Handlers) loadTradeBalances(ctx context.Context) (map[int64]*float64, error) {
	tz := h.localTZ()

	type anchorRow struct {
		Date    time.Time
		Balance float64
	}
	anchorsByAccount := map[int64][]anchorRow{}
	anchors, err := h.Queries.ListBalanceAnchors(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		return nil, fmt.Errorf("anchors: %w", err)
	}
	for _, a := range anchors {
		anchorsByAccount[a.AccountID] = append(anchorsByAccount[a.AccountID], anchorRow{Date: a.AnchorDate, Balance: a.Balance})
	}
	if len(anchorsByAccount) == 0 {
		return map[int64]*float64{}, nil
	}

	type tradeRow struct {
		ID       int64
		ExitTime time.Time
		NetPnl   float64
	}
	tradesByAccount := map[int64][]tradeRow{}
	tradeDeltas, err := h.Queries.ListClosedTradeDeltasOrdered(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		return nil, fmt.Errorf("trades: %w", err)
	}
	for _, r := range tradeDeltas {
		if r.ExitTime == nil {
			continue
		}
		if _, anchored := anchorsByAccount[r.AccountID]; !anchored {
			continue
		}
		tradesByAccount[r.AccountID] = append(tradesByAccount[r.AccountID], tradeRow{
			ID:       r.ID,
			ExitTime: *r.ExitTime,
			NetPnl:   r.NetPnl,
		})
	}

	out := make(map[int64]*float64, 256)
	for aid, anchors := range anchorsByAccount {
		trades := tradesByAccount[aid]
		if len(trades) == 0 {
			continue
		}

		// Cumulative net_pnl across the account's trades. cum[i] = sum of
		// NetPnl[0..i] inclusive.
		cum := make([]float64, len(trades))
		var s float64
		for i, t := range trades {
			s += t.NetPnl
			cum[i] = s
		}

		// For each anchor, find the largest trade index whose exit_time falls
		// within the anchor's end-of-day. That's the "include up to" index.
		anchorEndIdx := make([]int, len(anchors))
		for ai, a := range anchors {
			endOfDay := dayStart(a.Date, tz).Add(24*time.Hour - time.Nanosecond)
			idx := -1
			for j := 0; j < len(trades); j++ {
				if !trades[j].ExitTime.After(endOfDay) {
					idx = j
				} else {
					break
				}
			}
			anchorEndIdx[ai] = idx
		}

		// For each trade, pick the anchor: latest anchor whose date ≤ trade's
		// exit-day, or — when all anchors are in the future — the earliest.
		for i, t := range trades {
			tDay := dayStart(t.ExitTime, tz)
			ai := -1
			for k := 0; k < len(anchors); k++ {
				if !dayStart(anchors[k].Date, tz).After(tDay) {
					ai = k
				} else {
					break
				}
			}
			if ai == -1 {
				ai = 0 // earliest future anchor
			}
			anchor := anchors[ai]
			endIdx := anchorEndIdx[ai]

			var bal float64
			if i <= endIdx {
				// Trade is in this anchor's past.
				delta := cum[endIdx] - cum[i]
				bal = anchor.Balance - delta
			} else {
				// Trade is in this anchor's future.
				var prev float64
				if endIdx >= 0 {
					prev = cum[endIdx]
				}
				bal = anchor.Balance + (cum[i] - prev)
			}
			b := bal
			out[t.ID] = &b
		}
	}
	return out, nil
}

// AccountsAddBalance handles POST /accounts/{id}/balances — inserts (or
// updates, on UNIQUE conflict) a balance anchor at the given date.
func (h *Handlers) AccountsAddBalance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	accountID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	tz := h.localTZ()
	dateStr := strings.TrimSpace(r.FormValue("anchor_date"))
	if dateStr == "" {
		dateStr = time.Now().In(tz).Format("2006-01-02")
	}
	anchorDate, err := time.ParseInLocation("2006-01-02", dateStr, tz)
	if err != nil {
		http.Redirect(w, r, "/accounts?err="+url.QueryEscape("balance date must be YYYY-MM-DD"), http.StatusSeeOther)
		return
	}

	balStr := strings.TrimSpace(r.FormValue("balance"))
	balStr = strings.ReplaceAll(balStr, ",", "")
	balance, err := strconv.ParseFloat(balStr, 64)
	if err != nil {
		http.Redirect(w, r, "/accounts?err="+url.QueryEscape("balance must be a number"), http.StatusSeeOther)
		return
	}

	note := strings.TrimSpace(r.FormValue("note"))
	var notePtr *string
	if note != "" {
		notePtr = &note
	}

	// Upsert: if an anchor already exists for this (account, date) pair the
	// new value wins. Lets the user fix a same-day typo without first having
	// to delete the old row.
	if err := h.Queries.UpsertBalanceAnchor(ctx, models.UpsertBalanceAnchorParams{
		AccountID:  accountID,
		AnchorDate: anchorDate,
		Balance:    balance,
		Note:       notePtr,
	}); err != nil {
		h.serverError(w, fmt.Errorf("upsert anchor: %w", err))
		return
	}

	// HTMX clients get an in-place re-render of the cell + a toast. Plain
	// form submits (no JS, cURL) keep the 303 → flash redirect so the
	// no-JS path still works.
	if r.Header.Get("HX-Request") == "true" {
		h.renderBalanceCell(w, r, accountID, "Balance saved.")
		return
	}
	http.Redirect(w, r, "/accounts?flash=balanced", http.StatusSeeOther)
}

// BalanceDelete handles POST /balances/{anchor_id}/delete.
func (h *Handlers) BalanceDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("anchor_id")
	anchorID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	// For an HTMX response we need to know which account this anchor
	// belongs to so we can re-render that account's balance cell. Look
	// it up before deleting (the row is gone afterwards).
	var accountID int64
	scanErr := h.DB.QueryRowContext(ctx,
		`SELECT account_id FROM balance_anchors WHERE id = ?`, anchorID).Scan(&accountID)
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		h.serverError(w, fmt.Errorf("lookup anchor: %w", scanErr))
		return
	}

	if err := h.Queries.DeleteBalanceAnchor(ctx, models.DeleteBalanceAnchorParams{ID: anchorID, UserID: auth.MustUser(ctx).ID}); err != nil {
		h.serverError(w, fmt.Errorf("delete anchor: %w", err))
		return
	}

	if r.Header.Get("HX-Request") == "true" && accountID != 0 {
		h.renderBalanceCell(w, r, accountID, "Anchor deleted.")
		return
	}
	http.Redirect(w, r, "/accounts?flash=balanced", http.StatusSeeOther)
}

// renderBalanceCell is the HTMX response body for AccountsAddBalance and
// BalanceDelete. It re-renders the cell partial in place and emits a
// toast event via HX-Trigger so the page can flash a success banner
// without a full reload (which would wipe any unsaved dirty edits in
// the bulk-edit form on the rest of the page).
func (h *Handlers) renderBalanceCell(w http.ResponseWriter, r *http.Request, accountID int64, toastMsg string) {
	ctx := r.Context()
	acct, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: accountID, UserID: auth.MustUser(ctx).ID})
	if err != nil {
		h.serverError(w, fmt.Errorf("load account: %w", err))
		return
	}
	balances, err := h.loadBalances(ctx, time.Now())
	if err != nil {
		h.serverError(w, fmt.Errorf("load balances: %w", err))
		return
	}
	bal := balances[accountID] // zero-value AccountBalance is fine when no anchors

	// HX-Trigger payload: htmx fires a `toast` CustomEvent on document.body
	// with detail = the inner JSON value. The /accounts script bridges
	// that to window.toast().
	w.Header().Set("HX-Trigger", fmt.Sprintf(
		`{"toast":{"message":%q,"kind":"success"}}`, toastMsg))
	h.renderPartial(w, "_balance_cell.html", "balance_cell", map[string]any{
		"Account":  acct,
		"Balance":  bal,
		"Today":    time.Now().In(h.localTZ()).Format("2006-01-02"),
		"Currency": acct.Currency,
	})
}

