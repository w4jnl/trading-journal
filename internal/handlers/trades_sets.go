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
	// StopFlag names a plan that cannot mean what it says on this leg:
	// "entry beyond the stop" when the fill sits on the wrong side of it.
	StopFlag string
}

// setPlan is the set's view of its members' plans: one stop when every leg
// shares it (else the range), the summed risk, and the legs that carry a
// stop but no money (stub instrument).
type setPlan struct {
	Stop     *float64 // uniform stop, nil when none or mixed
	StopMin  *float64
	StopMax  *float64
	Mixed    bool
	Risk     float64 // summed planned risk
	Priced   int     // legs with a risk
	Unpriced int     // legs with a stop but no risk
	Exposure float64 // sum of qty x point value over priced legs: money per point for the set
}

// planOf folds the members' plans into the set's view and flags legs whose
// entry sits beyond their stop.
func planOf(members []setMember, pointValue map[string]float64) setPlan {
	var p setPlan
	for i := range members {
		m := &members[i]
		if pv, ok := pointValue[m.SymbolRoot]; ok {
			p.Exposure += m.Qty * pv
		}
		if m.PlannedStop != nil {
			st := *m.PlannedStop
			if p.StopMin == nil || st < *p.StopMin {
				p.StopMin = &st
			}
			if p.StopMax == nil || st > *p.StopMax {
				p.StopMax = &st
			}
			if (m.Direction == "long" && st >= m.EntryPrice) || (m.Direction == "short" && st <= m.EntryPrice) {
				m.StopFlag = "entry beyond the stop"
			}
			if m.PlannedRisk == nil {
				p.Unpriced++
			}
		}
		if m.PlannedRisk != nil {
			p.Risk += *m.PlannedRisk
			p.Priced++
		}
	}
	if p.StopMin != nil {
		if *p.StopMin == *p.StopMax {
			st := *p.StopMin
			p.Stop = &st
		} else {
			p.Mixed = true
		}
	}
	return p
}

// wrongSide reports a stop that does not protect the leg.
func wrongSide(direction string, entry, stop float64) bool {
	return (direction == "long" && stop >= entry) || (direction == "short" && stop <= entry)
}

// TradeSetPlanSave handles POST /trades/sets/{id}/plan. A stop price is a
// market level, so it goes on every leg and each leg's risk follows from
// its own entry, size and contract; a total risk alone splits by exposure
// (contracts times point value), which gives every leg the same distance
// from its own entry, snapped to its tick. With both posted, the one that
// differs from the stored plan is the edit; both changed, the stop wins.
// An empty form clears every leg.
func (h *Handlers) TradeSetPlanSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if _, err := h.Queries.GetTradeSet(ctx, models.GetTradeSetParams{ID: id, UserID: userID(ctx)}); err != nil {
		http.Error(w, "set not found", http.StatusNotFound)
		return
	}
	stop, ok := parsePositiveFloat(r.FormValue("stop"))
	if !ok {
		http.Error(w, "stop must be a positive price", http.StatusBadRequest)
		return
	}
	risk, ok := parsePositiveFloat(r.FormValue("risk"))
	if !ok {
		http.Error(w, "risk must be a positive amount", http.StatusBadRequest)
		return
	}
	rows, err := h.loadSetRows(ctx, id)
	if err != nil {
		h.serverError(w, fmt.Errorf("load set members: %w", err))
		return
	}
	insts := map[string]models.Instrument{}
	pointValue := map[string]float64{}
	for _, m := range rows {
		if _, seen := insts[m.SymbolRoot]; seen {
			continue
		}
		inst, err := h.Queries.GetInstrument(ctx, m.SymbolRoot)
		if err == nil {
			insts[m.SymbolRoot] = inst
			if pv, ok := positions.PointValue(inst); ok {
				pointValue[m.SymbolRoot] = pv
			}
		} else {
			insts[m.SymbolRoot] = models.Instrument{}
		}
	}
	members := make([]setMember, 0, len(rows))
	for _, m := range rows {
		members = append(members, setMember{tradeRow: m})
	}
	current := planOf(members, pointValue)

	// Which side was edited.
	var curRisk *float64
	if current.Priced > 0 {
		v := current.Risk
		curRisk = &v
	}
	stopEdited := !sameFloat(stop, current.Stop) || (current.Mixed && stop != nil)
	riskEdited := !sameFloat(risk, curRisk)
	mode := "clear"
	switch {
	case stop != nil && (risk == nil || stopEdited || !riskEdited):
		mode = "stop"
	case risk != nil:
		mode = "risk"
	}

	set := func(m tradeRow, st, rk *float64) error {
		_, err := h.Queries.SetTradePlan(ctx, models.SetTradePlanParams{PlannedStop: st, PlannedRisk: rk, ID: m.ID, UserID: userID(ctx)})
		return err
	}
	var msg string
	switch mode {
	case "clear":
		for _, m := range rows {
			if err := set(m, nil, nil); err != nil {
				h.serverError(w, fmt.Errorf("clear plan: %w", err))
				return
			}
		}
		msg = "Plan cleared on every leg"
	case "stop":
		total, unpriced := 0.0, 0
		for _, m := range rows {
			st := *stop
			rk := positions.PlannedRisk(m.EntryPrice, st, m.Qty, insts[m.SymbolRoot])
			if rk == nil {
				unpriced++
			} else {
				total += *rk
			}
			if err := set(m, &st, rk); err != nil {
				h.serverError(w, fmt.Errorf("set plan: %w", err))
				return
			}
		}
		msg = fmt.Sprintf("Stop %g on %d legs; risk %.2f in total", *stop, len(rows), total)
		if unpriced > 0 {
			msg += fmt.Sprintf(" (%d leg(s) unpriced: set the instrument's ticks)", unpriced)
		}
	case "risk":
		if current.Exposure <= 0 {
			http.Error(w, "no leg has instrument ticks to size a risk from; type a stop price instead", http.StatusBadRequest)
			return
		}
		dist := *risk / current.Exposure // points, the same on every leg
		total := 0.0
		var lo, hi float64
		first := true
		for _, m := range rows {
			pv, ok := pointValue[m.SymbolRoot]
			if !ok {
				continue // no ticks: cannot place a stop from money; leave the leg as is
			}
			share := dist * pv * m.Qty
			st := positions.PlannedStop(m.EntryPrice, share, m.Qty, m.Direction, insts[m.SymbolRoot])
			if st == nil {
				continue
			}
			rk := positions.PlannedRisk(m.EntryPrice, *st, m.Qty, insts[m.SymbolRoot])
			if rk != nil {
				total += *rk
			}
			if first || *st < lo {
				lo = *st
			}
			if first || *st > hi {
				hi = *st
			}
			first = false
			if err := set(m, st, rk); err != nil {
				h.serverError(w, fmt.Errorf("set plan: %w", err))
				return
			}
		}
		if lo == hi {
			msg = fmt.Sprintf("Risk %.2f split by exposure: stop %g on every leg (%.2f after tick rounding)", *risk, lo, total)
		} else {
			msg = fmt.Sprintf("Risk %.2f split by exposure: stops %g to %g (%.2f after tick rounding)", *risk, lo, hi, total)
		}
	}
	h.LiveData.Bump(userID(ctx))
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Trigger", `{"toast":{"message":`+strconv.Quote(msg)+`,"kind":"success"}}`)
		h.TradeSetDetail(w, r)
		return
	}
	http.Redirect(w, r, "/trades?view=sets&sheet=set:"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// loadSetRows is the set's members as trade rows, newest-entry last.
func (h *Handlers) loadSetRows(ctx context.Context, setID int64) ([]tradeRow, error) {
	return scanTradeRows(ctx, h.rdb(), tradeRowSelect+` WHERE m.set_id = ? AND `+ownsTradesSQL+` ORDER BY t.entry_time, t.id`, setID, userID(ctx))
}

// TradeSetDetail renders GET /trades/sets/{id} into the sheet.
func (h *Handlers) TradeSetDetail(w http.ResponseWriter, r *http.Request) {
	h.renderSetDetail(w, r, "")
}

// renderSetDetail renders the set sheet for the {id} path value; flash
// ("analyzed") confirms an action that re-rendered it in place.
func (h *Handlers) renderSetDetail(w http.ResponseWriter, r *http.Request, flash string) {
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

	// Tick size for the deltas; points when the instrument has none. Point
	// values per contract size the plan.
	var tick float64
	if inst, err := h.Queries.GetInstrument(ctx, set.SymbolRoot); err == nil && inst.TickSize != nil && *inst.TickSize > 0 {
		tick = *inst.TickSize
	}
	pointValue := map[string]float64{}
	for _, m := range rows {
		if _, seen := pointValue[m.SymbolRoot]; seen {
			continue
		}
		if inst, err := h.Queries.GetInstrument(ctx, m.SymbolRoot); err == nil {
			if pv, ok := positions.PointValue(inst); ok {
				pointValue[m.SymbolRoot] = pv
			}
		}
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
	plan := planOf(members, pointValue)
	data := map[string]any{
		"Set":           unit,
		"SetID":         id,
		"Members":       members,
		"BestFill":      best,
		"WorstFill":     worst,
		"TickSize":      tick,
		"Plan":          plan,
		"HasGroup":      hasGroup,
		"GroupID":       groupID,
		"ChartEnabled":  h.chartShow(linked),
		"ChartEndpoint": fmt.Sprintf("/charts/set/%d", id),
		"ChartCached":   h.chartCached(ctx, linked),
		"Back":          hxPageURL(r, "/trades?view=sets"),
		"Flash":         flash,
		"GroupBody":     "", // a string for aiAnalyzed even without a note
	}
	if hasGroup {
		data["AIPending"] = h.AIReviews.EntryPending(groupID)
		data["AIError"] = h.aiReviewError(r, groupID)
	}
	// The group note shows in the sheet, so a set review reads where it was
	// asked for.
	if hasGroup {
		if e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: groupID, UserID: userIDPtr(ctx)}); err == nil {
			data["GroupBody"] = e.Body
			if e.Tag != nil {
				data["GroupTag"] = *e.Tag
			}
			if labels, err := h.journalSvc().EntryLabels(ctx, userID(ctx), groupID); err == nil {
				data["GroupMistakes"], data["GroupEmotions"] = labels[journal.KindMistake], labels[journal.KindEmotion]
			}
		}
	}
	h.renderPartial(w, "_set_detail.html", "set_detail", data)
}

// ensureSetGroup returns the group note covering the set's trades, creating
// one with body when none does. sql.ErrNoRows when the set is not the
// caller's.
func (h *Handlers) ensureSetGroup(ctx context.Context, setID int64, body string) (int64, error) {
	if _, err := h.Queries.GetTradeSet(ctx, models.GetTradeSetParams{ID: setID, UserID: userID(ctx)}); err != nil {
		return 0, err
	}
	rows, err := h.loadSetRows(ctx, setID)
	if err != nil {
		return 0, fmt.Errorf("load set members: %w", err)
	}
	ids := make([]int64, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	eid, ok, err := h.journalSvc().FindGroupForTrades(ctx, userID(ctx), ids)
	if err != nil {
		return 0, fmt.Errorf("find group: %w", err)
	}
	if ok {
		return eid, nil
	}
	e, _, err := h.journalSvc().Create(ctx, userID(ctx), journal.CreateInput{Scope: "group", TradeIDs: ids, Body: body})
	if err != nil {
		return 0, err
	}
	return e.ID, nil
}

// TradeSetJournal handles POST /trades/sets/{id}/journal: opens the set's
// group note, creating it when none covers these trades yet.
func (h *Handlers) TradeSetJournal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	eid, err := h.ensureSetGroup(ctx, id, "")
	if err != nil {
		var ve apiValidationError
		switch {
		case errors.Is(err, sql.ErrNoRows):
			http.Error(w, "set not found", http.StatusNotFound)
		case errors.As(err, &ve):
			http.Error(w, ve.Msg, http.StatusBadRequest)
		default:
			h.serverError(w, fmt.Errorf("set group: %w", err))
		}
		return
	}
	target := fmt.Sprintf("/journal/group/%d?back=%s", eid, url.QueryEscape(hxPageURL(r, "/trades?view=sets")))
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
