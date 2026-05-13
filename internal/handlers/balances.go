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

	// Anchors per account, sorted by date ASC.
	type anchorRow struct {
		Date    time.Time
		Balance float64
	}
	anchorsByAccount := map[int64][]anchorRow{}
	{
		rows, err := h.DB.QueryContext(ctx, `
			SELECT account_id, anchor_date, balance
			FROM balance_anchors
			ORDER BY account_id, anchor_date ASC
		`)
		if err != nil {
			return nil, fmt.Errorf("anchors: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var aid int64
			var d time.Time
			var b float64
			if err := rows.Scan(&aid, &d, &b); err != nil {
				return nil, fmt.Errorf("scan anchor: %w", err)
			}
			anchorsByAccount[aid] = append(anchorsByAccount[aid], anchorRow{Date: d, Balance: b})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(anchorsByAccount) == 0 {
		return map[int64]*float64{}, nil
	}

	// Closed trades for every anchored account, ordered globally by exit_time
	// so we can group + walk per-account in one pass.
	type tradeRow struct {
		ID       int64
		ExitTime time.Time
		NetPnl   float64
	}
	tradesByAccount := map[int64][]tradeRow{}
	{
		rows, err := h.DB.QueryContext(ctx, `
			SELECT id, account_id, exit_time, COALESCE(net_pnl, 0)
			FROM trades
			WHERE status='closed' AND exit_time IS NOT NULL
			ORDER BY exit_time ASC
		`)
		if err != nil {
			return nil, fmt.Errorf("trades: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var aid int64
			var tr tradeRow
			if err := rows.Scan(&tr.ID, &aid, &tr.ExitTime, &tr.NetPnl); err != nil {
				return nil, fmt.Errorf("scan trade: %w", err)
			}
			if _, anchored := anchorsByAccount[aid]; !anchored {
				continue
			}
			tradesByAccount[aid] = append(tradesByAccount[aid], tr)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
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

