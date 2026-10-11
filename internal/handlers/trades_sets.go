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
	"github.com/w4jnl/trading-journal/internal/positions"
)

// The sets view of /trades: one row per decision. A copy set (internal/
// copyset) is one row with its member trades folded underneath; a trade in
// no set is a row of its own. Filters apply to the members, so a set shows
// the members that pass the filter and sums over them.

// setRow is one unit in the sets view.
type setRow struct {
	Key        string // "set:N" or the trade id: the row's data-trade-id
	SetID      *int64
	Members    []tradeRow
	Symbol     string // "MESZ6 x6 + ESZ6" style summary
	SymbolRoot string
	Direction  string
	EntryTime  time.Time
	ExitTime   *time.Time
	Accounts   int
	Qty        float64
	EntryAvg   float64
	EntryMin   float64
	EntryMax   float64
	ExitAvg    *float64
	GrossPnl   *float64
	Commission *float64
	NetPnl     *float64
	// PlannedRisk is the sum over members with a plan, so the set's R
	// reads like a trade's: net over risk.
	PlannedRisk *float64
	HasJournal  bool
	JournalTag  string
	PastStop    bool
	Retired     bool
}

// RMultiple is net over planned risk for the set, nil without a plan.
func (s setRow) RMultiple() *float64 { return positions.RMultiple(s.NetPnl, s.PlannedRisk) }

// IsSet reports a real copy set (as opposed to a lone trade).
func (s setRow) IsSet() bool { return s.SetID != nil }

// SetIDValue is the set id for templates (0 for a lone trade).
func (s setRow) SetIDValue() int64 {
	if s.SetID == nil {
		return 0
	}
	return *s.SetID
}

// Spread is the entry price range across members.
func (s setRow) Spread() float64 { return s.EntryMax - s.EntryMin }

// setGroupKeySQL is the unit of the sets view: the set id, or the trade id
// negated so it never collides with a set id.
const setGroupKeySQL = "COALESCE(m.set_id, -t.id)"

// setOrderSQL orders the page of units. Only entry time and net make sense
// over a group; anything else falls back to entry time.
func setOrderSQL(f tradeFilters) string {
	dir := "DESC"
	if f.Dir == "asc" {
		dir = "ASC"
	}
	switch f.Sort {
	case "net":
		return "SUM(t.net_pnl) " + dir + " NULLS LAST, MAX(t.entry_time) DESC, gk"
	case "entry":
		return "MAX(t.entry_time) " + dir + ", gk"
	}
	return "MAX(t.entry_time) DESC, gk"
}

func countTradeSetsFiltered(ctx context.Context, db *sql.DB, f tradeFilters) (int64, error) {
	where, args := buildWhere(f, userID(ctx))
	var n int64
	err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT `+setGroupKeySQL+`) FROM trades t LEFT JOIN trade_set_members m ON m.trade_id = t.id WHERE `+where, args...).Scan(&n)
	return n, err
}

// listTradeSetsFiltered pages the units, then loads their member rows.
func listTradeSetsFiltered(ctx context.Context, db *sql.DB, f tradeFilters) ([]setRow, error) {
	where, args := buildWhere(f, userID(ctx))
	pageArgs := append(append([]any{}, args...), tradesPageSize, (f.Page-1)*tradesPageSize)
	rs, err := db.QueryContext(ctx, `SELECT `+setGroupKeySQL+` AS gk FROM trades t LEFT JOIN trade_set_members m ON m.trade_id = t.id
		WHERE `+where+` GROUP BY gk ORDER BY `+setOrderSQL(f)+` LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, err
	}
	var keys []int64
	for rs.Next() {
		var k int64
		if err := rs.Scan(&k); err != nil {
			rs.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	ph := strings.Repeat("?,", len(keys))
	ph = ph[:len(ph)-1]
	rowArgs := append([]any{}, args...)
	for _, k := range keys {
		rowArgs = append(rowArgs, k)
	}
	rows, err := scanTradeRows(ctx, db, tradeRowSelect+` WHERE `+where+` AND `+setGroupKeySQL+` IN (`+ph+`) ORDER BY t.entry_time, t.id`, rowArgs...)
	if err != nil {
		return nil, err
	}
	byKey := map[int64]*setRow{}
	for _, r := range rows {
		k := -r.ID
		if r.SetID != nil {
			k = *r.SetID
		}
		sr, ok := byKey[k]
		if !ok {
			sr = &setRow{Key: r.GroupKey(), SetID: r.SetID, SymbolRoot: r.SymbolRoot, Direction: r.Direction, EntryTime: r.EntryTime, EntryMin: r.EntryPrice, EntryMax: r.EntryPrice}
			byKey[k] = sr
		}
		sr.add(r)
	}
	out := make([]setRow, 0, len(keys))
	for _, k := range keys {
		if sr, ok := byKey[k]; ok {
			sr.finish()
			out = append(out, *sr)
		}
	}
	return out, nil
}

// add folds one member into the unit.
func (s *setRow) add(r tradeRow) {
	s.Members = append(s.Members, r)
	s.Qty += r.Qty
	if r.EntryTime.Before(s.EntryTime) {
		s.EntryTime = r.EntryTime
	}
	if r.ExitTime != nil && (s.ExitTime == nil || r.ExitTime.After(*s.ExitTime)) {
		t := *r.ExitTime
		s.ExitTime = &t
	}
	s.EntryMin = min(s.EntryMin, r.EntryPrice)
	s.EntryMax = max(s.EntryMax, r.EntryPrice)
	addOpt(&s.GrossPnl, r.GrossPnl)
	addOpt(&s.Commission, r.Commission)
	addOpt(&s.NetPnl, r.NetPnl)
	addOpt(&s.PlannedRisk, r.PlannedRisk)
	if r.HasJournal || r.GroupCount > 0 {
		s.HasJournal = true
	}
	if s.JournalTag == "" {
		s.JournalTag = r.JournalTag
	}
	if r.PastStop() {
		s.PastStop = true
	}
	if r.AccountRetired {
		s.Retired = true
	}
}

// finish derives the averages and the symbol summary once all members are in.
func (s *setRow) finish() {
	accounts := map[int64]bool{}
	symbols := map[string]int{}
	var order []string
	var entrySum, exitSum float64
	exits := 0
	for _, m := range s.Members {
		accounts[m.AccountID] = true
		if symbols[m.Symbol] == 0 {
			order = append(order, m.Symbol)
		}
		symbols[m.Symbol]++
		entrySum += m.EntryPrice
		if m.ExitPrice != nil {
			exitSum += *m.ExitPrice
			exits++
		}
	}
	s.Accounts = len(accounts)
	if n := len(s.Members); n > 0 {
		s.EntryAvg = entrySum / float64(n)
	}
	if exits > 0 {
		v := exitSum / float64(exits)
		s.ExitAvg = &v
	}
	parts := make([]string, 0, len(order))
	for _, sym := range order {
		if n := symbols[sym]; n > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", sym, n))
		} else {
			parts = append(parts, sym)
		}
	}
	s.Symbol = strings.Join(parts, " + ")
}

func addOpt(dst **float64, v *float64) {
	if v == nil {
		return
	}
	if *dst == nil {
		x := *v
		*dst = &x
		return
	}
	**dst += *v
}

// tradesView reads the page's unit (trades or sets): the ?view= parameter
// when given (and remembered), else the remembered choice.
func (h *Handlers) tradesView(r *http.Request, fragment bool) string {
	ctx := r.Context()
	if v, ok := r.URL.Query()["view"]; ok {
		view := ""
		if len(v) > 0 && v[0] == "sets" {
			view = "sets"
		}
		if !fragment && h.Queries != nil {
			_ = h.Queries.UpsertViewPref(ctx, models.UpsertViewPrefParams{UserID: userID(ctx), Page: "trades.view", Value: view, UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
		}
		return view
	}
	if h.Queries != nil {
		if v, err := h.Queries.GetViewPref(ctx, models.GetViewPrefParams{UserID: userID(ctx), Page: "trades.view"}); err == nil && v == "sets" {
			return "sets"
		}
	}
	return ""
}

// viewQuery is the current filter query with the view switched and the
// page reset, for the Trades | Sets toggle.
func viewQuery(r *http.Request, view string) string {
	q := url.Values{}
	for k, vs := range r.URL.Query() {
		if k == "view" || k == "page" || k == "fragment" {
			continue
		}
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	if view != "" {
		q.Set("view", view)
	} else {
		q.Set("view", "trades")
	}
	return q.Encode()
}

// ---- the set sheet ----

// setMember is a member row of the set sheet with its distance from the
// set's average entry and exit, in ticks when the instrument has one.
type setMember struct {
	tradeRow
	EntryDelta *float64 // ticks (or points without a tick size), signed in the trade's favour
	ExitDelta  *float64
}

// loadSetRows is the set's members as trade rows, newest-entry last.
func (h *Handlers) loadSetRows(ctx context.Context, setID int64) ([]tradeRow, error) {
	return scanTradeRows(ctx, h.rdb(), tradeRowSelect+` WHERE m.set_id = ? AND `+ownsTradesSQL+` ORDER BY t.entry_time, t.id`, setID, userID(ctx))
}

// TradeSetDetail renders GET /trades/sets/{id} into the sheet.
func (h *Handlers) TradeSetDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	set, err := h.Queries.GetTradeSet(ctx, models.GetTradeSetParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		http.Error(w, "set not found", http.StatusNotFound)
		return
	}
	rows, err := h.loadSetRows(ctx, id)
	if err != nil {
		h.serverError(w, fmt.Errorf("load set members: %w", err))
		return
	}
	unit := setRow{Key: "set:" + strconv.FormatInt(id, 10), SetID: &set.ID, SymbolRoot: set.SymbolRoot, Direction: set.Direction}
	for i, m := range rows {
		if i == 0 {
			unit.EntryTime, unit.EntryMin, unit.EntryMax = m.EntryTime, m.EntryPrice, m.EntryPrice
		}
		unit.add(m)
	}
	unit.finish()

	// Tick size for the deltas; points when the instrument has none.
	var tick float64
	if inst, err := h.Queries.GetInstrument(ctx, set.SymbolRoot); err == nil && inst.TickSize != nil && *inst.TickSize > 0 {
		tick = *inst.TickSize
	}
	members := make([]setMember, 0, len(rows))
	ids := make([]int64, 0, len(rows))
	linked := make([]linkedTrade, 0, len(rows))
	for _, m := range rows {
		sm := setMember{tradeRow: m}
		// Favourable delta: a long that entered below the average, or a
		// short above it, reads positive.
		sign := 1.0
		if m.Direction == "long" {
			sign = -1
		}
		d := sign * (m.EntryPrice - unit.EntryAvg)
		if tick > 0 {
			d /= tick
		}
		sm.EntryDelta = &d
		if m.ExitPrice != nil && unit.ExitAvg != nil {
			e := -sign * (*m.ExitPrice - *unit.ExitAvg)
			if tick > 0 {
				e /= tick
			}
			sm.ExitDelta = &e
		}
		members = append(members, sm)
		ids = append(ids, m.ID)
		linked = append(linked, linkedTrade{ID: m.ID, Symbol: m.Symbol, SymbolRoot: m.SymbolRoot, Direction: m.Direction, Qty: m.Qty, AccountName: m.AccountName, EntryTime: m.EntryTime, ExitTime: m.ExitTime, GrossPnl: m.GrossPnl, Commission: m.Commission, NetPnl: m.NetPnl})
	}
	groupID, hasGroup, err := h.journalSvc().FindGroupForTrades(ctx, userID(ctx), ids)
	if err != nil {
		h.serverError(w, fmt.Errorf("find group: %w", err))
		return
	}
	var best, worst *setMember
	for i := range members {
		m := &members[i]
		if m.EntryDelta == nil {
			continue
		}
		if best == nil || *m.EntryDelta > *best.EntryDelta {
			best = m
		}
		if worst == nil || *m.EntryDelta < *worst.EntryDelta {
			worst = m
		}
	}
	if best != nil && worst != nil && *best.EntryDelta == *worst.EntryDelta {
		best, worst = nil, nil // every account got the same fill
	}
	h.renderPartial(w, "_set_detail.html", "set_detail", map[string]any{
		"Set":           unit,
		"SetID":         id,
		"Members":       members,
		"BestFill":      best,
		"WorstFill":     worst,
		"TickSize":      tick,
		"HasGroup":      hasGroup,
		"GroupID":       groupID,
		"ChartEnabled":  h.chartShow(linked),
		"ChartEndpoint": fmt.Sprintf("/charts/set/%d", id),
		"ChartCached":   h.chartCached(ctx, linked),
	})
}

// TradeSetJournal handles POST /trades/sets/{id}/journal: opens the set's
// group note, creating it when none covers these trades yet.
func (h *Handlers) TradeSetJournal(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAI(w, r) {
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := h.Queries.GetTradeSet(ctx, models.GetTradeSetParams{ID: id, UserID: userID(ctx)}); err != nil {
		http.Error(w, "set not found", http.StatusNotFound)
		return
	}
	rows, err := h.loadSetRows(ctx, id)
	if err != nil {
		h.serverError(w, fmt.Errorf("load set members: %w", err))
		return
	}
	ids := make([]int64, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	eid, ok, err := h.journalSvc().FindGroupForTrades(ctx, userID(ctx), ids)
	if err != nil {
		h.serverError(w, fmt.Errorf("find group: %w", err))
		return
	}
	if !ok {
		e, _, err := h.journalSvc().Create(ctx, userID(ctx), journal.CreateInput{Scope: "group", TradeIDs: ids})
		if err != nil {
			var ve apiValidationError
			if errors.As(err, &ve) {
				http.Error(w, ve.Msg, http.StatusBadRequest)
				return
			}
			h.serverError(w, fmt.Errorf("create group: %w", err))
			return
		}
		eid = e.ID
	}
	target := fmt.Sprintf("/journal/group/%d?back=%s", eid, url.QueryEscape("/trades?view=sets"))
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ChartSet serves the chart for a set: every member's markers on one panel.
func (h *Handlers) ChartSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := h.Queries.GetTradeSet(ctx, models.GetTradeSetParams{ID: id, UserID: userID(ctx)}); err != nil {
		http.NotFound(w, r)
		return
	}
	trades, err := scanLinkedTrades(ctx, h.rdb(), `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.qty, a.name,
		       t.entry_time, t.exit_time, t.gross_pnl, t.commission, t.net_pnl
		FROM trade_set_members m
		JOIN trades t   ON t.id = m.trade_id
		JOIN accounts a ON a.id = t.account_id
		WHERE m.set_id = ?
		ORDER BY t.exit_time ASC, t.id ASC`, id)
	if err != nil {
		h.serverError(w, fmt.Errorf("load set trades for chart: %w", err))
		return
	}
	h.writeChartResponse(w, r, trades)
}
