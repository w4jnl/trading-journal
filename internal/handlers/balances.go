package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

	// Anchors per account.
	anchorRows, err := h.DB.QueryContext(ctx, `
		SELECT id, account_id, anchor_date, balance, COALESCE(note, '')
		FROM balance_anchors
		ORDER BY account_id, anchor_date ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list balance_anchors: %w", err)
	}
	byAccount := map[int64][]BalanceAnchor{}
	for anchorRows.Next() {
		var a BalanceAnchor
		if err := anchorRows.Scan(&a.ID, &a.AccountID, &a.AnchorDate, &a.Balance, &a.Note); err != nil {
			anchorRows.Close()
			return nil, fmt.Errorf("scan anchor: %w", err)
		}
		byAccount[a.AccountID] = append(byAccount[a.AccountID], a)
	}
	if err := anchorRows.Close(); err != nil {
		return nil, err
	}

	// Closed-trade net_pnl per (account, exit-date-in-tz).
	type tradeDelta struct {
		AccountID int64
		ExitDate  time.Time // local-tz midnight
		NetPnL    float64
	}
	tradeRows, err := h.DB.QueryContext(ctx, `
		SELECT account_id, exit_time, COALESCE(net_pnl, 0)
		FROM trades
		WHERE status = 'closed' AND exit_time IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("list trades: %w", err)
	}
	var deltas []tradeDelta
	for tradeRows.Next() {
		var accountID int64
		var exitT time.Time
		var net float64
		if err := tradeRows.Scan(&accountID, &exitT, &net); err != nil {
			tradeRows.Close()
			return nil, fmt.Errorf("scan trade: %w", err)
		}
		y, m, d := exitT.In(tz).Date()
		deltas = append(deltas, tradeDelta{
			AccountID: accountID,
			ExitDate:  time.Date(y, m, d, 0, 0, 0, 0, tz),
			NetPnL:    net,
		})
	}
	if err := tradeRows.Close(); err != nil {
		return nil, err
	}

	todayDay := dayStart(today, tz)
	out := map[int64]AccountBalance{}
	for accountID, anchors := range byAccount {
		// Most recent anchor with anchor_date <= today. If none, fall back to
		// the earliest future anchor and back-fill from there.
		var anchor BalanceAnchor
		forward := true
		found := false
		for _, a := range anchors {
			if !dayStart(a.AnchorDate, tz).After(todayDay) {
				anchor = a
				found = true
			}
		}
		if !found {
			anchor = anchors[0]
			forward = false
		}
		anchorDay := dayStart(anchor.AnchorDate, tz)

		var delta float64
		for _, td := range deltas {
			if td.AccountID != accountID {
				continue
			}
			if forward {
				// Trades exited strictly after the anchor date, up to and
				// including today, add to the running balance.
				if td.ExitDate.After(anchorDay) && !td.ExitDate.After(todayDay) {
					delta += td.NetPnL
				}
			} else {
				// Anchor is in the future. To back-fill from there to today,
				// subtract trades that exited strictly after today up to and
				// including the anchor date.
				if td.ExitDate.After(todayDay) && !td.ExitDate.After(anchorDay) {
					delta -= td.NetPnL
				}
			}
		}

		out[accountID] = AccountBalance{
			Known:       true,
			Balance:     anchor.Balance + delta,
			AsOf:        todayDay,
			LatestDate:  anchorDay,
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
	_, err = h.DB.ExecContext(ctx, `
		INSERT INTO balance_anchors (account_id, anchor_date, balance, note)
		VALUES (?1, ?2, ?3, ?4)
		ON CONFLICT (account_id, anchor_date) DO UPDATE SET
			balance = excluded.balance,
			note = excluded.note
	`, accountID, anchorDate, balance, notePtr)
	if err != nil {
		h.serverError(w, fmt.Errorf("upsert anchor: %w", err))
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
	if _, err := h.DB.ExecContext(ctx, `DELETE FROM balance_anchors WHERE id = ?`, anchorID); err != nil {
		h.serverError(w, fmt.Errorf("delete anchor: %w", err))
		return
	}
	http.Redirect(w, r, "/accounts?flash=balanced", http.StatusSeeOther)
}

