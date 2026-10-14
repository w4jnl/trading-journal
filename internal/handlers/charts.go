package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// Trade-replay charts. Each view (trade detail, group/day/week journal) resolves
// its trades through an existing user-scoped loader, then this shared builder
// groups them by contract, picks a display timeframe, overlays entry/exit
// markers, and serves cached bars (fetching on ?load=1). See
// internal/marketdata for the cache + Yahoo source.

type chartMarker struct {
	T     int64   `json:"t"`     // unix seconds, fill time
	Price float64 `json:"price"` // fill price
	Side  string  `json:"side"`  // buy|sell
	Role  string  `json:"role"`  // entry|exit
	Qty   float64 `json:"qty"`   // qty of this fill used by the trade (summed across a folded group)
	// Count > 1 marks a folded marker: copy-trader fan-out fills the same
	// price at the same second on several accounts, which used to stack
	// identical arrows and labels on top of each other.
	Count int `json:"count,omitempty"`
}

type chartPanel struct {
	Contract     string   `json:"contract"`
	SymbolRoot   string   `json:"symbolRoot"`
	YahooSymbol  string   `json:"yahooSymbol"`
	Timeframe    string   `json:"timeframe"`    // default/selected display bar size
	BarTimeframe string   `json:"barTimeframe"` // granularity of the Bars array (finest available)
	Timeframes   []string `json:"timeframes"`   // selectable bar sizes (finest .. 1d)
	Direction    string   `json:"direction"`
	From         int64    `json:"from"` // the loaded window: the trade's whole trading day(s)
	To           int64    `json:"to"`
	// FocusFrom / FocusTo is the stretch around the trade the chart opens on
	// (and the PNG renders): the rest of the day is a scroll away.
	FocusFrom int64            `json:"focusFrom,omitempty"`
	FocusTo   int64            `json:"focusTo,omitempty"`
	EntryAvg  float64          `json:"entryAvg,omitempty"`
	ExitAvg   float64          `json:"exitAvg,omitempty"`
	Bars      []marketdata.Bar `json:"bars"`
	Markers   []chartMarker    `json:"markers"`
	Note      string           `json:"note,omitempty"`
}

type chartEnvelope struct {
	Status string       `json:"status"` // ok | needs_load | no_data
	Panels []chartPanel `json:"panels"`
}

// ChartTrade serves the chart for a single trade (the trade-detail modal and the
// standalone trade-journal page both target this).
func (h *Handlers) ChartTrade(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	trades, err := loadOneTrade(r.Context(), h.rdb(), id)
	if err != nil {
		h.serverError(w, fmt.Errorf("load trade for chart: %w", err))
		return
	}
	if len(trades) == 0 {
		http.NotFound(w, r)
		return
	}
	h.writeChartResponse(w, r, trades)
}

// ChartGroup serves charts for a group journal entry (one panel per contract).
func (h *Handlers) ChartGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// loadLinkedTrades is not user-scoped, so verify the entry is the caller's
	// before exposing its linked trades.
	var one int
	if err := h.rdb().QueryRowContext(r.Context(),
		`SELECT 1 FROM journal_entries WHERE id = ? AND user_id = ?`,
		id, userID(r.Context())).Scan(&one); err != nil {
		http.NotFound(w, r)
		return
	}
	trades, err := loadLinkedTrades(r.Context(), h.rdb(), id)
	if err != nil {
		h.serverError(w, fmt.Errorf("load linked trades for chart: %w", err))
		return
	}
	h.writeChartResponse(w, r, trades)
}

// ChartDay serves charts for a day journal entry (all trades exiting that day).
func (h *Handlers) ChartDay(w http.ResponseWriter, r *http.Request) {
	date, err := time.ParseInLocation("2006-01-02", r.PathValue("date"), h.localTZ())
	if err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	trades, err := loadDayTrades(r.Context(), h.rdb(), date)
	if err != nil {
		h.serverError(w, fmt.Errorf("load day trades for chart: %w", err))
		return
	}
	h.writeChartResponse(w, r, trades)
}

// ChartWeek serves charts for a week journal entry (all trades exiting that week).
func (h *Handlers) ChartWeek(w http.ResponseWriter, r *http.Request) {
	monday, _, _, err := parseISOWeek(r.PathValue("week"), h.localTZ())
	if err != nil {
		http.Error(w, "bad week", http.StatusBadRequest)
		return
	}
	trades, err := loadWeekTrades(r.Context(), h.rdb(), monday)
	if err != nil {
		h.serverError(w, fmt.Errorf("load week trades for chart: %w", err))
		return
	}
	h.writeChartResponse(w, r, trades)
}

func (h *Handlers) writeChartResponse(w http.ResponseWriter, r *http.Request, trades []linkedTrade) {
	load := r.URL.Query().Get("load") == "1"
	env := h.buildChartEnvelope(r.Context(), trades, load)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Markers + window are derived from the live trade set (which changes when
	// trades are linked/unlinked), so the response must never be browser-cached
	// even though the underlying bars are immutable.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(env); err != nil {
		h.logger().Error("encode chart", "err", err)
	}
}

// chartGroup is the set of a view's trades that share one Yahoo price series
// (a contract root; micros fold into the full-size series).
type chartGroup struct {
	yahooSym   string
	mapped     bool
	root       string
	contract   string
	direction  string
	tradeIDs   []int64
	firstEntry time.Time
	lastExit   time.Time
	hasOpen    bool
	multiRoot  bool // merges >1 instrument root (e.g. ES + MES) onto one chart
}

// effectiveExit returns lastExit, or now when any trade in the group is open.
func (g chartGroup) effectiveExit(now time.Time) time.Time {
	if g.hasOpen || g.lastExit.IsZero() {
		return now
	}
	return g.lastExit
}

// groupTradesForChart buckets trades by their Yahoo symbol (unmapped roots get
// their own bucket so they can render a "no data" note). Within a bucket the
// most common full Symbol / root / direction wins the labels.
func groupTradesForChart(trades []linkedTrade) []chartGroup {
	type acc struct {
		yahooSym                      string
		mapped                        bool
		rootCount, symCount, dirCount map[string]int
		tradeIDs                      []int64
		firstEntry, lastExit          time.Time
		hasOpen                       bool
	}
	buckets := map[string]*acc{}
	var order []string
	for _, t := range trades {
		ySym, ok := marketdata.YahooSymbol(t.SymbolRoot)
		key := ySym
		if !ok {
			key = "root:" + strings.ToUpper(t.SymbolRoot)
		}
		a := buckets[key]
		if a == nil {
			a = &acc{
				yahooSym:  ySym,
				mapped:    ok,
				rootCount: map[string]int{}, symCount: map[string]int{}, dirCount: map[string]int{},
			}
			buckets[key] = a
			order = append(order, key)
		}
		a.rootCount[t.SymbolRoot]++
		a.symCount[t.Symbol]++
		a.dirCount[t.Direction]++
		a.tradeIDs = append(a.tradeIDs, t.ID)
		if a.firstEntry.IsZero() || t.EntryTime.Before(a.firstEntry) {
			a.firstEntry = t.EntryTime
		}
		if t.ExitTime == nil {
			a.hasOpen = true
		} else if t.ExitTime.After(a.lastExit) {
			a.lastExit = *t.ExitTime
		}
	}
	groups := make([]chartGroup, 0, len(order))
	for _, key := range order {
		a := buckets[key]
		groups = append(groups, chartGroup{
			yahooSym:   a.yahooSym,
			mapped:     a.mapped,
			root:       topKey(a.rootCount),
			contract:   topKey(a.symCount),
			direction:  topKey(a.dirCount),
			tradeIDs:   a.tradeIDs,
			firstEntry: a.firstEntry,
			lastExit:   a.lastExit,
			hasOpen:    a.hasOpen,
			multiRoot:  len(a.rootCount) > 1,
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].contract < groups[j].contract })
	return groups
}

// topKey returns the most frequent key (ties broken alphabetically for stability).
func topKey(m map[string]int) string {
	best, bestN := "", -1
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

func (h *Handlers) buildChartEnvelope(ctx context.Context, trades []linkedTrade, load bool) chartEnvelope {
	if h.MarketData == nil {
		return chartEnvelope{Status: "no_data", Panels: []chartPanel{}}
	}
	now := time.Now()
	groups := groupTradesForChart(trades)
	panels := make([]chartPanel, 0, len(groups))
	anyOK, anyNeed := false, false
	for _, g := range groups {
		p, status := h.buildChartPanel(ctx, g, load, now)
		panels = append(panels, p)
		switch status {
		case "ok":
			anyOK = true
		case "needs_load":
			anyNeed = true
		}
	}
	status := "no_data"
	switch {
	case anyNeed:
		status = "needs_load"
	case anyOK:
		status = "ok"
	}
	return chartEnvelope{Status: status, Panels: panels}
}

func (h *Handlers) buildChartPanel(ctx context.Context, g chartGroup, load bool, now time.Time) (chartPanel, string) {
	markers, entryAvg, exitAvg := h.tradeMarkers(ctx, g.tradeIDs)
	panel := chartPanel{
		Contract:   g.contract,
		SymbolRoot: g.root,
		Direction:  g.direction,
		Markers:    markers,
		EntryAvg:   entryAvg,
		ExitAvg:    exitAvg,
	}
	if !g.mapped {
		panel.Note = "No market data available for " + g.root
		return panel, "no_data"
	}
	panel.YahooSymbol = g.yahooSym

	// The bar size the trade reads best at, and the stretch around it the
	// chart opens on; the bars loaded cover the whole trading day(s), so
	// the session around the trade is there without another fetch.
	displayTf, focusFrom, focusTo := marketdata.SelectTimeframe(g.firstEntry, g.effectiveExit(now), now)
	from, to := h.chartWindow(g, now)
	panel.From, panel.To = from.Unix(), to.Unix()
	panel.FocusFrom, panel.FocusTo = focusFrom.Unix(), focusTo.Unix()

	if load {
		if err := h.MarketData.EnsureWindow(ctx, g.yahooSym, from, to); err != nil {
			if errors.Is(err, marketdata.ErrRateLimited) {
				panel.Note = "Market data temporarily unavailable; try again shortly."
				return panel, "no_data"
			}
			h.logger().Warn("chart fetch", "symbol", g.yahooSym, "err", err)
		}
	}

	bars, readTf, covered, err := h.MarketData.BarsForWindow(ctx, g.yahooSym, from, to)
	if err != nil {
		h.logger().Warn("chart bars", "symbol", g.yahooSym, "err", err)
		return panel, "no_data"
	}
	if len(bars) == 0 {
		if covered || load {
			panel.Note = "No market data available for this period."
			return panel, "no_data"
		}
		return panel, "needs_load" // not cached yet; the load button will fetch
	}

	// Send bars at the finest stored granularity (capped for payload size); the
	// client rolls them up to the chosen bar size via the header selector. The
	// default selection is the duration-adaptive timeframe (never finer than the
	// data we have).
	const maxPayloadBars = 4500 // three days at one minute
	baseTf := readTf
	baseBars := bars
	if len(baseBars) > maxPayloadBars {
		for _, s := range marketdata.TimeframesFrom(readTf)[1:] {
			c := marketdata.Timeframe(s)
			baseBars = marketdata.AggregateTo(bars, c)
			baseTf = c
			if len(baseBars) <= maxPayloadBars {
				break
			}
		}
	}
	panel.Bars = baseBars
	panel.BarTimeframe = string(baseTf)
	panel.Timeframe = string(marketdata.Coarser(displayTf, baseTf))
	panel.Timeframes = marketdata.TimeframesFrom(baseTf)
	var notes []string
	if g.multiRoot {
		notes = append(notes, "Micro and full-size contracts are combined on one chart - they share the same underlying price.")
	}
	if g.contract != "" && !strings.EqualFold(g.contract, g.root) {
		notes = append(notes, "Continuous front-month series; candle basis may differ slightly from "+g.contract+".")
	}
	panel.Note = strings.Join(notes, " ")
	return panel, "ok"
}

// foldMarkers sorts markers by time and merges those identical in (time,
// price, side, role) into one carrying the summed qty and a count.
func foldMarkers(ms []chartMarker) []chartMarker {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].T != ms[j].T {
			return ms[i].T < ms[j].T
		}
		if ms[i].Price != ms[j].Price {
			return ms[i].Price < ms[j].Price
		}
		if ms[i].Side != ms[j].Side {
			return ms[i].Side < ms[j].Side
		}
		return ms[i].Role < ms[j].Role
	})
	out := ms[:0]
	for _, m := range ms {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.T == m.T && last.Price == m.Price && last.Side == m.Side && last.Role == m.Role {
				last.Qty += m.Qty
				if last.Count == 0 {
					last.Count = 1
				}
				last.Count++
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// tradeMarkers collects per-fill markers across a group's trades and the
// qty-weighted average entry/exit prices.
func (h *Handlers) tradeMarkers(ctx context.Context, tradeIDs []int64) (markers []chartMarker, entryAvg, exitAvg float64) {
	var entryPV, entryQ, exitPV, exitQ float64
	for _, id := range tradeIDs {
		execs, err := h.rq().ListExecutionsForTrade(ctx, id)
		if err != nil {
			h.logger().Warn("chart executions", "trade_id", id, "err", err)
			continue
		}
		for _, e := range execs {
			markers = append(markers, chartMarker{
				T:     e.ExecTime.Unix(),
				Price: e.Price,
				Side:  e.Side,
				Role:  e.Role,
				Qty:   e.QtyUsed,
			})
			switch e.Role {
			case "entry":
				entryPV += e.Price * e.QtyUsed
				entryQ += e.QtyUsed
			case "exit":
				exitPV += e.Price * e.QtyUsed
				exitQ += e.QtyUsed
			}
		}
	}
	markers = foldMarkers(markers)
	if entryQ > 0 {
		entryAvg = entryPV / entryQ
	}
	if exitQ > 0 {
		exitAvg = exitPV / exitQ
	}
	return markers, entryAvg, exitAvg
}

// chartShow reports whether a view has at least one trade on a charted
// (Yahoo-mapped) instrument. Views with no trades - or only stub/unmapped
// symbols - render no chart mount at all, so there's no pointless "Load chart"
// button on an empty journal entry.
func (h *Handlers) chartShow(trades []linkedTrade) bool {
	if h.MarketData == nil {
		return false
	}
	for _, t := range trades {
		if _, ok := marketdata.YahooSymbol(t.SymbolRoot); ok {
			return true
		}
	}
	return false
}

// chartCached reports whether every mapped panel for these trades is already
// cached, so the page can auto-render instead of showing a load button. Used by
// the page handlers' probe (see *Page handlers). Returns false when charting is
// disabled or any mapped window is uncovered.
func (h *Handlers) chartCached(ctx context.Context, trades []linkedTrade) bool {
	if h.MarketData == nil || len(trades) == 0 {
		return false
	}
	now := time.Now()
	for _, g := range groupTradesForChart(trades) {
		if !g.mapped {
			continue
		}
		from, to := h.chartWindow(g, now)
		covered, err := h.MarketData.WindowCovered(ctx, g.yahooSym, from, to)
		if err != nil || !covered {
			return false
		}
	}
	return true
}

// chartWindow is the stretch of bars a chart loads for a group: its whole
// trading day(s), in the cache's day bucketing, clipped at now.
func (h *Handlers) chartWindow(g chartGroup, now time.Time) (time.Time, time.Time) {
	loc := h.Timezone
	if h.MarketData != nil && h.MarketData.Loc != nil {
		loc = h.MarketData.Loc
	}
	if loc == nil {
		loc = time.UTC
	}
	return marketdata.DayWindow(g.firstEntry, g.effectiveExit(now), now, loc)
}
