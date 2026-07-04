// admin_transactions.go — /admin/transactions: the reconcile editor. Filter, create,
// edit, and (soft-)delete raw executions for the acting user's accounts, then re-derive
// trades via the FIFO builder. Executions are the system of record; trades are rebuilt.
// All routes are mounted behind auth.RequireAdmin in main.go.
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// txnRow is one execution row in the reconcile table (raw query, joined to accounts).
type txnRow struct {
	ID           int64
	AccountID    int64
	AccountRef   string
	AccountName  string
	Symbol       string
	SymbolRoot   string
	Side         string
	Qty          float64
	Price        float64
	ExecTime     time.Time
	BrokerExecID string
	Manual       bool
	Deleted      bool
	Edited       bool
	Inline       bool // owning account is TopstepX trades-CSV (read-only here)
}

// txnFilter is the current filter state (also round-tripped through redirects).
type txnFilter struct {
	AccountID      int64
	Symbol         string
	From           string // YYYY-MM-DD
	To             string // YYYY-MM-DD
	Side           string // "", buy, sell
	IncludeDeleted bool
}

// AdminTransactionsPage: GET /admin/transactions - filter form + open-position summary +
// the filtered executions table + a create form.
func (h *Handlers) AdminTransactionsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)
	tz := h.localTZ()

	accounts, err := h.Queries.ListAccounts(ctx, uid)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	openPositions, err := h.Queries.ListOpenPositions(ctx, uid)
	if err != nil {
		h.serverError(w, fmt.Errorf("list open positions: %w", err))
		return
	}

	f := parseTxnFilter(r)
	rows, err := h.listExecutionsFiltered(ctx, uid, f)
	if err != nil {
		h.serverError(w, fmt.Errorf("list executions: %w", err))
		return
	}

	h.renderPage(w, r, "admin_transactions.html", map[string]any{
		"NavActive":     "admin_transactions",
		"BackHref":      validBackHref(r),
		"Accounts":      accounts,
		"OpenPositions": openPositions,
		"Rows":          rows,
		"Filter":        f,
		"FilterQuery":   f.query().Encode(), // carried on POST actions to preserve the view
		"NowLocal":      time.Now().In(tz).Format("2006-01-02T15:04"),
		"Flash":         r.URL.Query().Get("flash"),
		"FlashErr":      r.URL.Query().Get("err"),
	})
}

// query renders the filter as url.Values (for a form action's query string).
func (f txnFilter) query() url.Values {
	v := url.Values{}
	if f.AccountID != 0 {
		v.Set("account_id", strconv.FormatInt(f.AccountID, 10))
	}
	if f.Symbol != "" {
		v.Set("symbol", f.Symbol)
	}
	if f.From != "" {
		v.Set("from", f.From)
	}
	if f.To != "" {
		v.Set("to", f.To)
	}
	if f.Side != "" {
		v.Set("side", f.Side)
	}
	if f.IncludeDeleted {
		v.Set("deleted", "1")
	}
	return v
}

// listExecutionsFiltered runs the dynamic reconcile query (raw SQL - sqlc can't do
// optional filters). Always scoped to the user's own accounts.
func (h *Handlers) listExecutionsFiltered(ctx context.Context, uid int64, f txnFilter) ([]txnRow, error) {
	var b strings.Builder
	b.WriteString(`SELECT e.id, e.account_id, a.account_ref, a.name, e.symbol, e.symbol_root,
		e.side, e.qty, e.price, e.exec_time, e.broker_exec_id, e.manual,
		e.deleted_at, e.edited_at, a.commission_source
		FROM executions e JOIN accounts a ON a.id = e.account_id
		WHERE a.user_id = ?`)
	args := []any{uid}
	if f.AccountID != 0 {
		b.WriteString(" AND e.account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.Symbol != "" {
		b.WriteString(" AND e.symbol = ?")
		args = append(args, f.Symbol)
	}
	if f.Side == "buy" || f.Side == "sell" {
		b.WriteString(" AND e.side = ?")
		args = append(args, f.Side)
	}
	if f.From != "" {
		b.WriteString(" AND substr(e.exec_time,1,10) >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		b.WriteString(" AND substr(e.exec_time,1,10) <= ?")
		args = append(args, f.To)
	}
	if !f.IncludeDeleted {
		b.WriteString(" AND e.deleted_at IS NULL")
	}
	b.WriteString(" ORDER BY e.exec_time DESC, e.id DESC LIMIT 1000")

	rows, err := h.DB.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []txnRow
	for rows.Next() {
		var t txnRow
		var manual int64
		var deletedAt, editedAt *time.Time
		var commissionSrc string
		if err := rows.Scan(&t.ID, &t.AccountID, &t.AccountRef, &t.AccountName, &t.Symbol,
			&t.SymbolRoot, &t.Side, &t.Qty, &t.Price, &t.ExecTime, &t.BrokerExecID, &manual,
			&deletedAt, &editedAt, &commissionSrc); err != nil {
			return nil, err
		}
		t.Manual = manual != 0
		t.Deleted = deletedAt != nil
		t.Edited = editedAt != nil
		t.Inline = commissionSrc == "inline"
		out = append(out, t)
	}
	return out, rows.Err()
}

// AdminTransactionCreate: POST /admin/transactions - add a hand-entered fill.
func (h *Handlers) AdminTransactionCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)
	in, msg := h.parseTxnForm(r)
	if msg != "" {
		h.redirectTransactions(w, r, "", msg)
		return
	}
	acct, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: in.accountID, UserID: uid})
	if err != nil {
		h.redirectTransactions(w, r, "", "account not found")
		return
	}
	if acct.CommissionSource == "inline" {
		h.redirectTransactions(w, r, "", "cannot hand-edit executions on a trades-CSV (inline) account")
		return
	}
	// Resolve the broker id: blank auto-generates a MANUAL- id; a value the user typed (e.g.
	// a real broker ticket) is used verbatim, so a later re-import of that same ticket dedups
	// against this row instead of duplicating it. It must be unique within the account.
	brokerID := strings.TrimSpace(r.PostFormValue("broker_exec_id"))
	if brokerID == "" {
		brokerID = manualBrokerExecID()
	} else if _, err := h.Queries.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{AccountID: in.accountID, BrokerExecID: brokerID}); err == nil {
		h.redirectTransactions(w, r, "", "broker id "+brokerID+" already exists on this account")
		return
	}
	// Auto-stub the instrument so the FK never fails on an unseen root (same as the importer).
	if err := h.Queries.UpsertInstrumentStub(ctx, models.UpsertInstrumentStubParams{SymbolRoot: in.symbolRoot}); err != nil {
		h.serverError(w, fmt.Errorf("upsert instrument: %w", err))
		return
	}
	if _, err := h.Queries.InsertManualExecution(ctx, models.InsertManualExecutionParams{
		AccountID:    in.accountID,
		BrokerExecID: brokerID,
		Symbol:       in.symbol,
		SymbolRoot:   in.symbolRoot,
		Side:         in.side,
		Qty:          in.qty,
		Price:        in.price,
		ExecTime:     in.execTime,
	}); err != nil {
		h.serverError(w, fmt.Errorf("insert execution: %w", err))
		return
	}
	h.rebuildAndRedirect(w, r, uid, in.accountID, in.symbol, "created a "+in.side+" fill")
}

// acctSym keys the set of FIFO queues a bulk save touched (one rebuild each, deduped).
type acctSym struct {
	accountID int64
	symbol    string
}

// AdminTransactionsBulkSave: POST /admin/transactions/save - one submission for every row
// on the page (the single Save behind the Edit toggle). Row inputs are named
// "txn.<id>.<field>" (symbol / symbol_root / side / qty / price / exec_time) plus
// "txn.<id>.delete" and "txn.<id>.restore" checkboxes. Only rows that actually changed are
// written; each touched (account, symbol) FIFO queue is rebuilt once. Invalid rows are
// skipped silently (not a 400 for the whole batch), mirroring AccountsBulkSave.
func (h *Handlers) AdminTransactionsBulkSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	type rowIn struct {
		symbol, root, side, qty, price, execTime string
		del, restore                             bool
	}
	byID := map[int64]*rowIn{}
	for key, vals := range r.PostForm {
		if !strings.HasPrefix(key, "txn.") || len(vals) == 0 {
			continue
		}
		rest := key[len("txn."):]
		dot := strings.IndexByte(rest, '.')
		if dot < 0 {
			continue
		}
		id, err := strconv.ParseInt(rest[:dot], 10, 64)
		if err != nil {
			continue
		}
		ri := byID[id]
		if ri == nil {
			ri = &rowIn{}
			byID[id] = ri
		}
		v := strings.TrimSpace(vals[0])
		switch rest[dot+1:] {
		case "symbol":
			ri.symbol = strings.ToUpper(v)
		case "symbol_root":
			ri.root = strings.ToUpper(v)
		case "side":
			ri.side = strings.ToLower(v)
		case "qty":
			ri.qty = v
		case "price":
			ri.price = v
		case "exec_time":
			ri.execTime = v
		case "delete":
			ri.del = v == "on" || v == "1"
		case "restore":
			ri.restore = v == "on" || v == "1"
		}
	}

	tz := h.localTZ()
	touched := map[acctSym]bool{}
	changed := 0
	for id, ri := range byID {
		cur, err := h.Queries.GetExecutionForUser(ctx, models.GetExecutionForUserParams{ID: id, UserID: uid})
		if err != nil || cur.CommissionSource == "inline" {
			continue // unknown / foreign / inline account - skip
		}
		switch {
		case ri.del && cur.DeletedAt == nil:
			if err := h.Queries.SoftDeleteExecution(ctx, models.SoftDeleteExecutionParams{ID: id, UserID: uid}); err == nil {
				touched[acctSym{cur.AccountID, cur.Symbol}] = true
				changed++
			}
			continue
		case ri.restore && cur.DeletedAt != nil:
			if err := h.Queries.UndeleteExecution(ctx, models.UndeleteExecutionParams{ID: id, UserID: uid}); err == nil {
				touched[acctSym{cur.AccountID, cur.Symbol}] = true
				changed++
			}
			continue
		}
		// Field edit. Validate; skip the row silently if unusable.
		qty, err1 := strconv.ParseFloat(ri.qty, 64)
		price, err2 := strconv.ParseFloat(ri.price, 64)
		execTime, err3 := parseExecTime(ri.execTime, tz)
		if ri.symbol == "" || ri.root == "" || (ri.side != "buy" && ri.side != "sell") ||
			err1 != nil || qty <= 0 || err2 != nil || price <= 0 || err3 != nil {
			continue
		}
		// Only write when something actually changed - compare exec_time at the displayed
		// second precision so a sub-second-only difference doesn't churn a needless rebuild.
		if ri.symbol == cur.Symbol && ri.root == cur.SymbolRoot && ri.side == cur.Side &&
			qty == cur.Qty && price == cur.Price &&
			ri.execTime == cur.ExecTime.In(tz).Format("2006-01-02T15:04:05") {
			continue
		}
		if err := h.Queries.UpsertInstrumentStub(ctx, models.UpsertInstrumentStubParams{SymbolRoot: ri.root}); err != nil {
			h.serverError(w, fmt.Errorf("upsert instrument: %w", err))
			return
		}
		if err := h.Queries.UpdateExecution(ctx, models.UpdateExecutionParams{
			Symbol: ri.symbol, SymbolRoot: ri.root, Side: ri.side, Qty: qty, Price: price,
			ExecTime: execTime, ID: id, UserID: uid,
		}); err != nil {
			h.serverError(w, fmt.Errorf("update execution %d: %w", id, err))
			return
		}
		touched[acctSym{cur.AccountID, cur.Symbol}] = true // old queue
		if ri.symbol != cur.Symbol {
			touched[acctSym{cur.AccountID, ri.symbol}] = true // new queue if the contract moved
		}
		changed++
	}

	for k := range touched {
		if err := h.ImportSvc.RebuildTradesForAccountSymbol(ctx, uid, k.accountID, k.symbol); err != nil {
			h.logger().Warn("reconcile bulk rebuild", "account", k.accountID, "symbol", k.symbol, "err", err)
		}
	}
	msg := "no changes"
	if changed > 0 {
		msg = fmt.Sprintf("saved %d change(s)", changed)
	}
	h.redirectTransactions(w, r, msg, "")
}

// rebuildAndRedirect re-derives trades for (account, symbol) then redirects back to the
// filtered view with a flash showing the resulting net position (so the user immediately
// sees whether the account is now flat).
func (h *Handlers) rebuildAndRedirect(w http.ResponseWriter, r *http.Request, uid, accountID int64, symbol, action string) {
	ctx := r.Context()
	if err := h.ImportSvc.RebuildTradesForAccountSymbol(ctx, uid, accountID, symbol); err != nil {
		h.serverError(w, fmt.Errorf("rebuild trades: %w", err))
		return
	}
	net := h.netPosition(ctx, accountID, symbol)
	status := "flat"
	if net != 0 {
		status = fmt.Sprintf("net %+g", net)
	}
	h.redirectTransactions(w, r, fmt.Sprintf("%s - %s is now %s", action, symbol, status), "")
}

// netPosition is the signed open quantity for (account, symbol), excluding tombstones.
func (h *Handlers) netPosition(ctx context.Context, accountID int64, symbol string) float64 {
	var net *float64
	_ = h.DB.QueryRowContext(ctx,
		`SELECT SUM(CASE WHEN side='buy' THEN qty ELSE -qty END) FROM executions
		 WHERE account_id=? AND symbol=? AND deleted_at IS NULL`, accountID, symbol).Scan(&net)
	if net == nil {
		return 0
	}
	return *net
}

// ---- form parsing / helpers ----

type txnInput struct {
	accountID  int64
	symbol     string
	symbolRoot string
	side       string
	qty        float64
	price      float64
	execTime   time.Time
}

// parseTxnForm validates the create/edit form. Returns a non-empty message on the first
// problem (shown to the user), else a filled txnInput.
func (h *Handlers) parseTxnForm(r *http.Request) (txnInput, string) {
	var in txnInput
	if err := r.ParseForm(); err != nil {
		return in, "bad form"
	}
	in.accountID, _ = strconv.ParseInt(r.PostFormValue("account_id"), 10, 64)
	if in.accountID == 0 {
		return in, "pick an account"
	}
	in.symbol = strings.ToUpper(strings.TrimSpace(r.PostFormValue("symbol")))
	if in.symbol == "" {
		return in, "contract symbol is required (e.g. MESU26)"
	}
	in.symbolRoot = strings.ToUpper(strings.TrimSpace(r.PostFormValue("symbol_root")))
	if in.symbolRoot == "" {
		return in, "instrument root is required (e.g. MES)"
	}
	in.side = strings.ToLower(strings.TrimSpace(r.PostFormValue("side")))
	if in.side != "buy" && in.side != "sell" {
		return in, "side must be buy or sell"
	}
	var err error
	if in.qty, err = strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("qty")), 64); err != nil || in.qty <= 0 {
		return in, "quantity must be a positive number"
	}
	if in.price, err = strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("price")), 64); err != nil || in.price <= 0 {
		return in, "price must be a positive number"
	}
	in.execTime, err = parseExecTime(strings.TrimSpace(r.PostFormValue("exec_time")), h.localTZ())
	if err != nil {
		return in, "invalid date/time"
	}
	return in, ""
}

// parseExecTime reads the datetime-local form value in the trading timezone and returns
// UTC (executions store UTC; the FIFO builder orders on it as a string, so all rows must
// share the same offset - UTC - to sort correctly alongside imported fills).
func parseExecTime(v string, tz *time.Location) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, v, tz); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable time %q", v)
}

// manualBrokerExecID mints a unique, identifiable id for a hand-entered fill. The MANUAL-
// prefix never collides with a numeric broker ticket and marks the row as hand-made.
func manualBrokerExecID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("MANUAL-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func parseTxnFilter(r *http.Request) txnFilter {
	q := r.URL.Query()
	f := txnFilter{
		Symbol:         strings.ToUpper(strings.TrimSpace(q.Get("symbol"))),
		From:           strings.TrimSpace(q.Get("from")),
		To:             strings.TrimSpace(q.Get("to")),
		Side:           strings.ToLower(strings.TrimSpace(q.Get("side"))),
		IncludeDeleted: q.Get("deleted") == "1",
	}
	f.AccountID, _ = strconv.ParseInt(q.Get("account_id"), 10, 64)
	return f
}

// redirectTransactions returns to the list, preserving the active filter so an action
// keeps the user in place, plus a flash or error. The filter travels in the POST action's
// query string (FilterQuery) - read from there, NOT the form body, so a create/edit's own
// symbol/side fields don't hijack the filter.
func (h *Handlers) redirectTransactions(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	v := url.Values{}
	q := r.URL.Query()
	for _, k := range []string{"account_id", "symbol", "from", "to", "side", "deleted"} {
		if val := q.Get(k); val != "" {
			v.Set(k, val)
		}
	}
	if flash != "" {
		v.Set("flash", flash)
	}
	if errMsg != "" {
		v.Set("err", errMsg)
	}
	target := "/admin/transactions"
	if enc := v.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
