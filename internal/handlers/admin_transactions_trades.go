// admin_transactions_trades.go - the "Trades" mode of the reconcile editor
// (/admin/transactions?view=trades). Where the fills mode edits executions one
// row at a time, this mode works on the FIFO-matched trades: one row per closed
// trade with its fills underneath, a delete checkbox that tombstones every fill
// of the trade in one go, and a "Deleted trades" section from which such a
// group is restored as a unit. Trades are derived, so both actions end in the
// same per-(account, symbol) rebuild the fills mode uses.
package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// reconTradesLimit caps the trade list (newest first); the page says so when hit.
const reconTradesLimit = 500

// reconTrade is one closed trade in the trades reconcile table.
type reconTrade struct {
	ID             int64
	AccountID      int64
	AccountName    string
	AccountFirm    *string
	AccountRetired bool
	Inline         bool // trades-CSV account: FIFO rebuild would clobber it, so no delete
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
	JournalEntryID *int64
	GroupCount     int64
	// FlipLinks are other trades that share one of this trade's fills (a
	// position flip). Deleting this trade tombstones the shared fill too and
	// re-pairs the neighbour from its remaining fills.
	FlipLinks []int64
	// OpenShare: one of the fills also feeds a still-open lot; deleting the
	// trade removes that open remainder as well.
	OpenShare bool
	// Sig fingerprints the trade across rebuilds (ids are reused after a
	// rebuild; entry_time is the identity the builder itself relinks on).
	Sig   string
	Fills []reconFill
}

// reconFill is one execution as it participates in a trade (or, for deleted
// groups, on its own: Role empty, QtyUsed 0).
type reconFill struct {
	ID           int64
	Role         string
	QtyUsed      float64
	Side         string
	Qty          float64
	Price        float64
	ExecTime     time.Time
	BrokerExecID string
	OrderID      string
	Method       string
	MethodLabel  string
	Manual       bool
	Edited       bool
	Provisional  bool
	SharedWith   []int64 // other trades this same fill participates in
}

// reconDeletedGroup is one trade-level delete: the fills tombstoned together.
type reconDeletedGroup struct {
	GroupID     string
	AccountID   int64
	AccountName string
	Symbol      string
	SymbolRoot  string
	FillCount   int
	First       time.Time
	Last        time.Time
	BuyQty      float64
	SellQty     float64
	DeletedAt   time.Time
	Fills       []reconFill
}

// tradeSig is the per-trade fingerprint carried in the form (see reconTrade.Sig).
func tradeSig(entry time.Time) string { return entry.UTC().Format(time.RFC3339Nano) }

// listTradesForReconcile lists the user's closed trades matching the filter,
// newest first, with the per-trade journal signals. Symbol matches either the
// contract or its root so typing "ES" finds ESU6. Dates select trades that
// OVERLAP the window (a trade open across the boundary counts on both sides).
func (h *Handlers) listTradesForReconcile(ctx context.Context, uid int64, f txnFilter) ([]reconTrade, error) {
	var b strings.Builder
	b.WriteString(`SELECT t.id, t.account_id, a.name, a.firm, a.commission_source, a.active,
		t.symbol, t.symbol_root, t.direction, t.qty, t.entry_price, t.exit_price,
		t.entry_time, t.exit_time, t.gross_pnl, t.commission, t.net_pnl,
		(SELECT je.id FROM journal_entries je WHERE je.trade_id = t.id LIMIT 1) AS journal_entry_id,
		(SELECT COUNT(*) FROM journal_entry_trades jet WHERE jet.trade_id = t.id) AS group_count
		FROM trades t JOIN accounts a ON a.id = t.account_id
		WHERE a.user_id = ? AND t.status = 'closed'`)
	args := []any{uid}
	if f.AccountID != 0 {
		b.WriteString(" AND t.account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.Symbol != "" {
		b.WriteString(" AND (t.symbol = ? OR t.symbol_root = ?)")
		args = append(args, f.Symbol, f.Symbol)
	}
	if f.From != "" {
		b.WriteString(" AND substr(COALESCE(t.exit_time, t.entry_time),1,10) >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		b.WriteString(" AND substr(t.entry_time,1,10) <= ?")
		args = append(args, f.To)
	}
	// Outcome by net P&L sign; stub instruments (NULL P&L) match none of them.
	switch f.Outcome {
	case "win":
		b.WriteString(" AND t.net_pnl > 0")
	case "loss":
		b.WriteString(" AND t.net_pnl < 0")
	case "flat":
		b.WriteString(" AND t.net_pnl = 0")
	}
	b.WriteString(" ORDER BY t.entry_time DESC, t.id DESC LIMIT ?")
	args = append(args, reconTradesLimit)

	rows, err := h.DB.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reconTrade
	for rows.Next() {
		var t reconTrade
		var commissionSrc string
		var active int64
		if err := rows.Scan(&t.ID, &t.AccountID, &t.AccountName, &t.AccountFirm, &commissionSrc, &active,
			&t.Symbol, &t.SymbolRoot, &t.Direction, &t.Qty, &t.EntryPrice, &t.ExitPrice,
			&t.EntryTime, &t.ExitTime, &t.GrossPnl, &t.Commission, &t.NetPnl,
			&t.JournalEntryID, &t.GroupCount); err != nil {
			return nil, err
		}
		t.Inline = commissionSrc == "inline"
		t.AccountRetired = active == 0
		t.Sig = tradeSig(t.EntryTime)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := h.attachFills(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachFills loads every fill of the listed trades in one query, groups them
// per trade, and derives the flip / open-lot warnings: a fill that appears in
// two listed trades links them; a fill whose used quantity falls short of its
// size is looked up on its own (its partner may be outside the list, or the
// remainder may be a still-open lot).
func (h *Handlers) attachFills(ctx context.Context, trades []reconTrade) error {
	if len(trades) == 0 {
		return nil
	}
	idx := make(map[int64]int, len(trades))
	ph := make([]string, 0, len(trades))
	args := make([]any, 0, len(trades))
	for i, t := range trades {
		idx[t.ID] = i
		ph = append(ph, "?")
		args = append(args, t.ID)
	}
	rows, err := h.DB.QueryContext(ctx, `SELECT te.trade_id, te.role, te.qty_used,
		e.id, e.side, e.qty, e.price, e.exec_time, e.broker_exec_id, e.order_id,
		e.import_method, e.manual, e.edited_at
		FROM trade_executions te JOIN executions e ON e.id = te.execution_id
		WHERE te.trade_id IN (`+strings.Join(ph, ",")+`)
		ORDER BY te.trade_id, te.role, e.exec_time, e.id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type use struct {
		tradeIdx int
		fillIdx  int
	}
	uses := map[int64][]use{}      // execution id -> where it appears
	usedQty := map[int64]float64{} // execution id -> summed qty_used in the list
	execQty := map[int64]float64{}
	for rows.Next() {
		var tradeID int64
		var f reconFill
		var orderID *string
		var manual int64
		var editedAt *time.Time
		if err := rows.Scan(&tradeID, &f.Role, &f.QtyUsed, &f.ID, &f.Side, &f.Qty, &f.Price, &f.ExecTime,
			&f.BrokerExecID, &orderID, &f.Method, &manual, &editedAt); err != nil {
			return err
		}
		if orderID != nil {
			f.OrderID = *orderID
		}
		f.MethodLabel = methodLabel(f.Method)
		f.Provisional = f.Method == "mw_email"
		f.Manual = manual != 0
		f.Edited = editedAt != nil
		ti := idx[tradeID]
		trades[ti].Fills = append(trades[ti].Fills, f)
		uses[f.ID] = append(uses[f.ID], use{ti, len(trades[ti].Fills) - 1})
		usedQty[f.ID] += f.QtyUsed
		execQty[f.ID] = f.Qty
	}
	if err := rows.Err(); err != nil {
		return err
	}
	const eps = 1e-9
	for execID, where := range uses {
		partners := map[int64]bool{}
		for _, u := range where {
			partners[trades[u.tradeIdx].ID] = true
		}
		openShare := false
		if usedQty[execID]+eps < execQty[execID] {
			// Part of this fill lives outside the listed trades: another trade
			// (not listed) or an open lot.
			all, err := h.Queries.ListTradesForExecution(ctx, execID)
			if err != nil {
				return err
			}
			var total float64
			for _, r := range all {
				partners[r.TradeID] = true
				total += r.QtyUsed
			}
			openShare = total+eps < execQty[execID]
		}
		for _, u := range where {
			self := trades[u.tradeIdx].ID
			for p := range partners {
				if p != self {
					trades[u.tradeIdx].Fills[u.fillIdx].SharedWith = append(trades[u.tradeIdx].Fills[u.fillIdx].SharedWith, p)
					trades[u.tradeIdx].FlipLinks = append(trades[u.tradeIdx].FlipLinks, p)
				}
			}
			if openShare {
				trades[u.tradeIdx].OpenShare = true
			}
		}
	}
	for i := range trades {
		trades[i].FlipLinks = uniqSorted(trades[i].FlipLinks)
		for j := range trades[i].Fills {
			trades[i].Fills[j].SharedWith = uniqSorted(trades[i].Fills[j].SharedWith)
		}
	}
	return nil
}

func uniqSorted(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := map[int64]bool{}
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// listDeletedTradeGroups aggregates the tombstoned fills of trade-level
// deletes into one row per group and applies the page filter in Go (groups
// are few). A group stays listed while any of its fills is still tombstoned.
func (h *Handlers) listDeletedTradeGroups(ctx context.Context, uid int64, f txnFilter) ([]reconDeletedGroup, error) {
	rows, err := h.Queries.ListDeletedGroupExecutions(ctx, uid)
	if err != nil {
		return nil, err
	}
	byGroup := map[string]*reconDeletedGroup{}
	var order []string
	for _, r := range rows {
		if r.DeletedGroup == nil {
			continue
		}
		g := byGroup[*r.DeletedGroup]
		if g == nil {
			g = &reconDeletedGroup{GroupID: *r.DeletedGroup, AccountID: r.AccountID, AccountName: r.AccountName,
				Symbol: r.Symbol, SymbolRoot: r.SymbolRoot, First: r.ExecTime, Last: r.ExecTime}
			if r.DeletedAt != nil {
				g.DeletedAt = *r.DeletedAt
			}
			byGroup[g.GroupID] = g
			order = append(order, g.GroupID)
		}
		g.FillCount++
		if r.ExecTime.Before(g.First) {
			g.First = r.ExecTime
		}
		if r.ExecTime.After(g.Last) {
			g.Last = r.ExecTime
		}
		if r.Side == "buy" {
			g.BuyQty += r.Qty
		} else {
			g.SellQty += r.Qty
		}
		fill := reconFill{ID: r.ID, Side: r.Side, Qty: r.Qty, Price: r.Price, ExecTime: r.ExecTime,
			BrokerExecID: r.BrokerExecID, Method: r.ImportMethod, MethodLabel: methodLabel(r.ImportMethod),
			Provisional: r.ImportMethod == "mw_email", Manual: r.Manual != 0, Edited: r.EditedAt != nil}
		if r.OrderID != nil {
			fill.OrderID = *r.OrderID
		}
		g.Fills = append(g.Fills, fill)
	}
	out := make([]reconDeletedGroup, 0, len(order))
	for _, gid := range order {
		g := byGroup[gid]
		if f.AccountID != 0 && g.AccountID != f.AccountID {
			continue
		}
		if f.Symbol != "" && g.Symbol != f.Symbol && g.SymbolRoot != f.Symbol {
			continue
		}
		if f.From != "" && g.Last.UTC().Format("2006-01-02") < f.From {
			continue
		}
		if f.To != "" && g.First.UTC().Format("2006-01-02") > f.To {
			continue
		}
		out = append(out, *g)
	}
	return out, nil
}

// deletedGroupIDRe validates group ids coming back from the form.
var deletedGroupIDRe = regexp.MustCompile(`^tg_[A-Za-z0-9_-]{22}$`)

// newDeletedGroupID mints the id stamped on every fill of one trade-level delete.
func newDeletedGroupID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "tg_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// AdminTransactionsTradesSave: POST /admin/transactions/trades/save - the
// trades view's single submit. "trade.<id>.delete" (with "trade.<id>.sig")
// tombstones every fill of that trade under one fresh deleted_group;
// "group.<gid>.restore" clears the tombstones of a group. Each touched
// (account, symbol) queue is rebuilt once. Tombstones happen in one
// transaction per trade; rebuilds run in their own transactions as they do for
// the fills view (single-writer SQLite, and a tombstone is durable, so a failed
// rebuild is recoverable by restore or re-save).
func (h *Handlers) AdminTransactionsTradesSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	deletes := map[int64]bool{}
	sigs := map[int64]string{}
	var restores []string
	for key, vals := range r.PostForm {
		if len(vals) == 0 {
			continue
		}
		v := strings.TrimSpace(vals[0])
		switch {
		case strings.HasPrefix(key, "trade."):
			rest := key[len("trade."):]
			dot := strings.IndexByte(rest, '.')
			if dot < 0 {
				continue
			}
			id, err := strconv.ParseInt(rest[:dot], 10, 64)
			if err != nil {
				continue
			}
			switch rest[dot+1:] {
			case "delete":
				if v == "on" || v == "1" {
					deletes[id] = true
				}
			case "sig":
				sigs[id] = v
			}
		case strings.HasPrefix(key, "group.") && strings.HasSuffix(key, ".restore"):
			gid := strings.TrimSuffix(key[len("group."):], ".restore")
			if (v == "on" || v == "1") && deletedGroupIDRe.MatchString(gid) {
				restores = append(restores, gid)
			}
		}
	}
	ids := make([]int64, 0, len(deletes))
	for id := range deletes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	sort.Strings(restores)

	touched := map[acctSym]bool{}
	var errs []string
	deletedTrades, deletedFills, restored := 0, 0, 0

	for _, id := range ids {
		trade, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: uid})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				errs = append(errs, fmt.Sprintf("trade #%d no longer exists (trades were rebuilt) - reload and retry", id))
				continue
			}
			h.serverError(w, fmt.Errorf("load trade %d: %w", id, err))
			return
		}
		if sigs[id] != tradeSig(trade.EntryTime) {
			errs = append(errs, fmt.Sprintf("trade #%d changed since the page loaded - reload and retry", id))
			continue
		}
		acct, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: trade.AccountID, UserID: uid})
		if err != nil {
			errs = append(errs, fmt.Sprintf("trade #%d: account not found", id))
			continue
		}
		if acct.CommissionSource == "inline" {
			errs = append(errs, fmt.Sprintf("trade #%d is on a read-only trades-CSV account", id))
			continue
		}
		n, err := h.tombstoneTrade(ctx, uid, id)
		if err != nil {
			h.serverError(w, fmt.Errorf("delete trade %d: %w", id, err))
			return
		}
		touched[acctSym{trade.AccountID, trade.Symbol}] = true
		deletedTrades++
		deletedFills += int(n)
	}

	for _, gid := range restores {
		g := gid
		fills, err := h.Queries.ListExecutionsInDeletedGroup(ctx, models.ListExecutionsInDeletedGroupParams{DeletedGroup: &g, UserID: uid})
		if err != nil {
			h.serverError(w, fmt.Errorf("load group %s: %w", gid, err))
			return
		}
		if len(fills) == 0 {
			errs = append(errs, "deleted trade group not found (already restored?)")
			continue
		}
		for _, f := range fills {
			touched[acctSym{f.AccountID, f.Symbol}] = true
		}
		if _, err := h.Queries.UndeleteExecutionGroup(ctx, models.UndeleteExecutionGroupParams{DeletedGroup: &g, UserID: uid}); err != nil {
			h.serverError(w, fmt.Errorf("restore group %s: %w", gid, err))
			return
		}
		restored++
	}

	for k := range touched {
		if err := h.ImportSvc.RebuildTradesForAccountSymbol(ctx, uid, k.accountID, k.symbol); err != nil {
			h.logger().Warn("reconcile trades rebuild", "account", k.accountID, "symbol", k.symbol, "err", err)
			errs = append(errs, "rebuild failed for "+k.symbol+" - see server log")
		}
	}
	if deletedTrades > 0 || restored > 0 {
		h.LiveData.Bump(uid)
	}
	var parts []string
	if deletedTrades > 0 {
		parts = append(parts, fmt.Sprintf("deleted %d trade(s) (%d fills)", deletedTrades, deletedFills))
	}
	if restored > 0 {
		parts = append(parts, fmt.Sprintf("restored %d trade(s)", restored))
	}
	flash := "no changes"
	if len(parts) > 0 {
		flash = strings.Join(parts, "; ")
	}
	h.redirectTransactions(w, r, flash, strings.Join(errs, "; "))
}

// tombstoneTrade soft-deletes every fill of one trade under a fresh group id,
// all-or-nothing, and returns how many fills it actually removed (fills already
// tombstoned in the fills view keep their state and are not counted).
func (h *Handlers) tombstoneTrade(ctx context.Context, uid, tradeID int64) (int64, error) {
	gid, err := newDeletedGroupID()
	if err != nil {
		return 0, err
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := h.Queries.WithTx(tx)
	fills, err := q.ListExecutionsForTrade(ctx, tradeID)
	if err != nil {
		return 0, err
	}
	seen := map[int64]bool{}
	var n int64
	for _, f := range fills {
		if seen[f.ID] { // a flip fill is listed once per role
			continue
		}
		seen[f.ID] = true
		g := gid
		rows, err := q.SoftDeleteExecutionInGroup(ctx, models.SoftDeleteExecutionInGroupParams{DeletedGroup: &g, ID: f.ID, UserID: uid})
		if err != nil {
			return 0, err
		}
		n += rows
	}
	return n, tx.Commit()
}

// reconRow is one line of the trades table: either a live trade or, with
// "show deleted" on, a trade-level delete group rendered in place at its
// chronological position (the fills view shows deleted fills inline the same
// way; a separate section below a 500-row table is where nobody looks).
type reconRow struct {
	Trade *reconTrade
	Group *reconDeletedGroup
	At    time.Time
}

// mergeReconRows interleaves live trades (by entry time) and deleted groups
// (by first fill), newest first.
func mergeReconRows(trades []reconTrade, groups []reconDeletedGroup) []reconRow {
	rows := make([]reconRow, 0, len(trades)+len(groups))
	for i := range trades {
		rows = append(rows, reconRow{Trade: &trades[i], At: trades[i].EntryTime})
	}
	for i := range groups {
		rows = append(rows, reconRow{Group: &groups[i], At: groups[i].First})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	return rows
}
