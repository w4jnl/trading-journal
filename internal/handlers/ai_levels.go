package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/chartpng"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/levelmap"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// ---- AI levels on an OPEN position -------------------------------------------
//
// The one place a stop and target suggestion is honest: the position is
// open, the bars end now and no outcome exists to peek at. The model gets
// what the app computed - the position, the recent bars (and the chart
// image when the user sends charts), the day plan and what its max loss
// still allows, the lab's best distances for the instrument, the trader's
// strategies and recent lessons - and answers in a fixed JSON shape. The
// app snaps the prices to tick, rejects levels on the wrong side of the
// entry or outside the bars, recomputes risk and R itself, and stores the
// result in ai_outputs (kind position_levels, ref = the plan's anchor)
// with the time and the last price sealed in. Nothing is written to the
// plan until the trader clicks "Use as plan" (source 'ai'); after the
// close the suggestion stays on the trade sheet next to what the trader
// chose, so the review and the lab can compare them fairly.

const aiKindPositionLevels = "position_levels"

// aiLevelsPrompt is the system prompt of a levels suggestion.
const aiLevelsPrompt = `You help a futures day trader place the stop and the profit target on a position that is OPEN RIGHT NOW. The user message is JSON: the position (direction, average entry, size, time open, unrealized P&L at the last price), the session as of now - its structure (prior RTH range and close, overnight range, opening range, session extremes, VWAP, ATR, the last swing highs and lows) with the structural levels nearest the last price, and a multi-timeframe window of bars newest last (15m over the last sessions, 5m over the last hours, 1m over the last minutes; the chart image shows the recent bars when attached) - the trader's day plan (bias, levels they marked, the max loss per account and how much of it the day still allows), the lab's evidence for this instrument (the stop and target distances in points that paid best over the trader's own past decisions, with the win rate), the trader's strategies, their recent lessons, and the plan they recorded so far, if any.

Rules:
- Propose levels, never actions on orders: the trader places every order themselves.
- The stop goes on the losing side of the entry at a place the structure justifies (beyond a swing, a level, a range edge), not a round number of points; say which. Prefer a stop the lab's evidence supports when the structure allows.
- The target goes on the winning side at the next level that matters; a second target is optional. Both must lie within or near the range of the bars shown.
- Give the money at risk only as a sanity check; the app recomputes it.
- Never say or assume the trader has no stop: an empty recorded plan only means none was recorded here.
- Answer ONLY with JSON, no prose around it, in exactly this shape (null where you have nothing):
{"stop": 7755.0, "target": 7790.0, "target2": null, "scaleOut": null, "trail": "none", "reasons": {"stop": "one sentence", "target": "one sentence"}, "summary": "two or three sentences for the trader"}
trail is one of "none", "breakeven-at-1r" (move the stop to the entry once price is one risk unit in favour), "behind-last-swing". scaleOut is the fraction (0-1) to take off at the first target when you propose a second one.`

// aiLevels is what the model proposed, checked and priced by the app.
type aiLevels struct {
	Stop     float64            `json:"stop"`
	Target   float64            `json:"target"`
	Target2  *float64           `json:"target2"`
	ScaleOut *float64           `json:"scaleOut"`
	Trail    string             `json:"trail"`
	Reasons  map[string]string  `json:"reasons"`
	Summary  string             `json:"summary"`
	Risk     *float64           `json:"risk"`   // at the position's size when suggested
	R1       *float64           `json:"r1"`     // reward:risk to the first target
	R2       *float64           `json:"r2"`     // to the second
	Entry    float64            `json:"entry"`  // the average entry it was made for
	Qty      float64            `json:"qty"`    // and the size
	Last     *float64           `json:"last"`   // the last price at that moment
	At       time.Time          `json:"at"`     // when
	BarsTf   string             `json:"barsTf"` // the bar size it saw, "" without bars
	Warnings []string           `json:"warnings,omitempty"`
	Extra    map[string]float64 `json:"-"`
}

// aiLevelsRaw is the model's answer before checks.
type aiLevelsRaw struct {
	Stop     *float64          `json:"stop"`
	Target   *float64          `json:"target"`
	Target2  *float64          `json:"target2"`
	ScaleOut *float64          `json:"scaleOut"`
	Trail    string            `json:"trail"`
	Reasons  map[string]string `json:"reasons"`
	Summary  string            `json:"summary"`
}

// parseAILevels reads the model's JSON (code fences tolerated) and checks it
// against the position: the stop on the losing side of the entry, the
// targets on the winning side, every level within the bars' range widened
// by a margin (when bars were shown), prices snapped to the tick. Risk and
// reward:risk are the app's own numbers.
func parseAILevels(text string, direction string, entry, qty float64, inst models.Instrument, low, high float64) (aiLevels, error) {
	s := strings.TrimSpace(text)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 {
		s = s[:j+1]
	}
	var raw aiLevelsRaw
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return aiLevels{}, fmt.Errorf("the model did not answer in the expected shape: %w", err)
	}
	if raw.Stop == nil || *raw.Stop <= 0 || raw.Target == nil || *raw.Target <= 0 {
		return aiLevels{}, errors.New("the model gave no stop or no target")
	}
	snap := func(p float64) float64 {
		if inst.TickSize != nil && *inst.TickSize > 0 {
			return math.Round(p / *inst.TickSize) * *inst.TickSize
		}
		return p
	}
	out := aiLevels{Stop: snap(*raw.Stop), Target: snap(*raw.Target), Trail: raw.Trail, Reasons: raw.Reasons, Summary: strings.TrimSpace(raw.Summary), Entry: entry, Qty: qty}
	if raw.Target2 != nil && *raw.Target2 > 0 {
		t2 := snap(*raw.Target2)
		out.Target2 = &t2
	}
	if raw.ScaleOut != nil && *raw.ScaleOut > 0 && *raw.ScaleOut < 1 {
		out.ScaleOut = raw.ScaleOut
	}
	switch out.Trail {
	case "none", "breakeven-at-1r", "behind-last-swing":
	default:
		out.Trail = "none"
	}
	long := direction == "long"
	if (long && out.Stop >= entry) || (!long && out.Stop <= entry) {
		return aiLevels{}, fmt.Errorf("the model put the stop (%g) on the wrong side of the entry (%g)", out.Stop, entry)
	}
	if (long && out.Target <= entry) || (!long && out.Target >= entry) {
		return aiLevels{}, fmt.Errorf("the model put the target (%g) on the wrong side of the entry (%g)", out.Target, entry)
	}
	if out.Target2 != nil && ((long && *out.Target2 <= out.Target) || (!long && *out.Target2 >= out.Target)) {
		out.Target2 = nil
	}
	if high > low {
		// Room beyond the bars: a quarter of their range, and never less
		// than half a percent of price, so a quiet window does not refuse
		// a stop a few points outside it while a level far away still fails.
		margin := math.Max((high-low)*0.25, entry*0.005)
		for _, p := range []float64{out.Stop, out.Target} {
			if p < low-margin || p > high+margin {
				return aiLevels{}, fmt.Errorf("the model's level %g lies outside the bars shown (%g to %g)", p, low, high)
			}
		}
		if out.Target2 != nil && (*out.Target2 < low-margin || *out.Target2 > high+margin) {
			out.Target2 = nil
		}
	}
	if r := positions.PlannedRisk(entry, out.Stop, qty, inst); r != nil {
		out.Risk = r
		dist := math.Abs(entry - out.Stop)
		r1 := math.Abs(out.Target-entry) / dist
		out.R1 = &r1
		if out.Target2 != nil {
			r2 := math.Abs(*out.Target2-entry) / dist
			out.R2 = &r2
		}
	}
	return out, nil
}

// aiLevelsRef is the ai_outputs ref of a position's suggestion: its plan
// anchor, so the suggestion meets the trade the cycle becomes.
func aiLevelsRef(accountID int64, symbol string, openExecID int64) string {
	return strconv.FormatInt(accountID, 10) + ":" + symbol + ":" + strconv.FormatInt(openExecID, 10)
}

// positionBars is the recent bar window of an open position for the model:
// the panel (for the image) and the compact window, nil when no bars.
func (h *Handlers) positionBars(ctx context.Context, pos positionView, now time.Time) (*chartPanel, map[string]any) {
	if h.MarketData == nil {
		return nil, nil
	}
	yahoo, ok := marketdata.YahooSymbol(pos.SymbolRoot)
	if !ok {
		return nil, nil
	}
	g := chartGroup{yahooSym: yahoo, mapped: true, root: pos.SymbolRoot, contract: pos.Symbol, direction: pos.Direction, firstEntry: pos.OpenedAt, hasOpen: true}
	panel, status := h.buildChartPanel(ctx, g, true, now)
	if status != "ok" || len(panel.Bars) == 0 {
		return nil, nil
	}
	for _, f := range pos.CycleFills {
		panel.Markers = append(panel.Markers, chartMarker{T: f.ExecTime.Unix(), Price: f.Price, Side: f.Side, Role: "entry", Qty: f.Qty})
	}
	if pos.AvgEntry != nil {
		panel.EntryAvg = *pos.AvgEntry
	}
	tz := h.localTZ()
	return &panel, aiBarWindow(panel, tz)
}

// suggestPositionLevels runs one suggestion for the position and stores it.
func (h *Handlers) suggestPositionLevels(ctx context.Context, accountID int64, symbol string) error {
	client, model, err := h.aiFor(ctx)
	if err != nil {
		return err
	}
	rows, err := h.rq().ListOpenPositions(ctx, userID(ctx))
	if err != nil {
		return err
	}
	var pos *positionView
	for _, v := range h.openPositionRows(ctx, rows) {
		if v.AccountID == accountID && v.Symbol == symbol {
			v := v
			pos = &v
		}
	}
	if pos == nil || pos.AvgEntry == nil || pos.Qty == 0 {
		return errors.New("the position is flat now")
	}
	now := time.Now()
	tz := h.localTZ()
	inst, _ := h.Queries.GetInstrument(ctx, pos.SymbolRoot)

	payload := map[string]any{
		"timezone": tz.String(),
		"position": map[string]any{
			"account": pos.AccountName, "symbol": pos.Symbol, "instrument": pos.SymbolRoot, "direction": pos.Direction,
			"qty": pos.Qty, "avgEntry": *pos.AvgEntry, "openedAt": pos.OpenedAt.In(tz).Format("2006-01-02 15:04"),
			"minutesOpen": int(now.Sub(pos.OpenedAt).Minutes()), "fills": pos.Fills,
			"lastPrice": pos.LastPrice, "unrealized": pos.Unrealized, "usdPerPoint": pos.PointValue, "tickSize": pos.TickSize,
		},
	}
	if pos.HasPlan() {
		payload["recordedPlan"] = map[string]any{"stop": pos.Stop, "risk": pos.Risk, "target": pos.Target, "thesis": pos.Thesis}
	}
	if pos.InSet() {
		payload["copiedDecision"] = map[string]any{"legs": len(pos.Legs) + 1, "setRisk": pos.SetRisk}
	}
	// The session as a trader reads it (structure plus a multi-timeframe
	// window from the cached one-minute bars), with the chart panel kept
	// for the image and for the range the levels must fall in.
	panel, window := h.positionBars(ctx, *pos, now)
	low, high := 0.0, 0.0
	barsTf := ""
	if market, ok := levelmap.MarketOf(pos.SymbolRoot); ok {
		if m, err := h.levelMap(ctx, market, levelmap.TradingDay(now, h.chicago()), now); err == nil && m != nil && len(m.Zones) > 0 {
			payload["dayMap"] = m.Brief(derefOr(pos.AvgEntry, 0), 6, 3)
		}
	}
	if session := h.aiSession(ctx, pos.SymbolRoot, now); session != nil {
		payload["session"] = session
		if tf, ok := session["barsStoredAt"].(string); ok {
			barsTf = tf
		}
	} else if window != nil {
		payload["bars"] = window
		barsTf, _ = window["tf"].(string)
	}
	if panel != nil {
		for _, b := range panel.Bars {
			if low == 0 || b.Low < low {
				low = b.Low
			}
			if b.High > high {
				high = b.High
			}
		}
	}
	dayKey := now.In(tz).Format("2006-01-02")
	if plan, err := dayplan.Get(ctx, h.rq(), userID(ctx), dayKey); err == nil && plan != nil {
		dp := map[string]any{"bias": plan.Bias, "levels": plan.Levels, "maxTrades": plan.MaxTrades, "stopTime": plan.StopTime, "maxLoss": plan.MaxLoss, "notes": plan.Notes}
		if plan.MaxLoss != nil {
			realised := h.realisedToday(ctx, pos.AccountID, dayKey, tz)
			left := *plan.MaxLoss + realised
			if left < 0 {
				left = 0
			}
			dp["realisedTodayOnAccount"] = realised
			dp["lossBudgetLeft"] = left
		}
		payload["dayPlan"] = dp
	}
	if lab, err := h.runLab(ctx, labFilters{Root: labRootOf(pos.SymbolRoot)}); err == nil && lab != nil && lab.Decisions > 0 {
		ev := map[string]any{"decisions": lab.Decisions, "usdPerPoint": lab.PointUSD, "baseline": lab.Baseline}
		if lab.BestFound {
			ev["bestStopPoints"], ev["bestTargetPoints"] = lab.BestStop, lab.BestTgt
		}
		payload["labEvidence"] = ev
	}
	if strats, err := h.Queries.ListActiveStrategies(ctx, userID(ctx)); err == nil && len(strats) > 0 {
		list := make([]map[string]any, 0, len(strats))
		for _, s := range strats {
			list = append(list, map[string]any{"name": s.Name, "playbook": aiTruncate(s.Body, 500)})
		}
		payload["strategies"] = list
	}
	if rows, err := h.Queries.ListRecentLessons(ctx, models.ListRecentLessonsParams{UserID: userID(ctx), EntryID: nil, Limit: 5}); err == nil && len(rows) > 0 {
		var lessons []string
		for _, l := range rows {
			lessons = append(lessons, l.Text)
		}
		payload["recentLessons"] = lessons
	}
	user, _ := jsonIndent(payload)
	var png []byte
	if panel != nil && h.userSendsChart(ctx) {
		if b, err := chartpng.Render(panelToPNG(*panel, tz), chartpng.Options{}); err == nil {
			png = b
			user = "The attached image is this position's chart (candles with the fills marked).\n\n" + user
		}
	}
	text, err := aiGenerate(ctx, client, aiLevelsPrompt+aiLanguageLine(h.userAILanguage(ctx)), user, png)
	if err != nil {
		return fmt.Errorf("ai generate: %w", err)
	}
	lv, err := parseAILevels(text, pos.Direction, *pos.AvgEntry, pos.Qty, inst, low, high)
	if err != nil {
		return err
	}
	lv.Last, lv.At, lv.BarsTf = pos.LastPrice, now, barsTf
	if barsTf == "" {
		lv.Warnings = append(lv.Warnings, "no bars were available: the levels were judged from the position and the plan only")
	}
	data, _ := json.Marshal(lv)
	return h.Queries.UpsertAIOutput(ctx, models.UpsertAIOutputParams{
		UserID: userID(ctx), Kind: aiKindPositionLevels, Ref: aiLevelsRef(pos.AccountID, pos.Symbol, pos.Opened.ID),
		Body: lv.Summary, Data: string(data), Model: model.Label(aiWordsFor("")),
	})
}

// labRootOf folds a contract root the way the lab lists it (micros under
// the full-size root: MES under ES).
func labRootOf(root string) string {
	return strings.TrimSuffix(copyset.Key(root), "=F")
}

// realisedToday is the account's net on the day's closed trades.
func (h *Handlers) realisedToday(ctx context.Context, accountID int64, day string, tz *time.Location) float64 {
	d, err := time.ParseInLocation("2006-01-02", day, tz)
	if err != nil {
		return 0
	}
	var net float64
	_ = h.rdb().QueryRowContext(ctx, `SELECT COALESCE(SUM(net_pnl), 0) FROM trades WHERE account_id = ? AND status = 'closed' AND exit_time >= ? AND exit_time < ?`,
		accountID, d.UTC(), d.AddDate(0, 0, 1).UTC()).Scan(&net)
	return net
}

// positionAIView is the suggestion as the sheet shows it.
type positionAIView struct {
	Pending  bool
	Error    string
	Has      bool
	Levels   aiLevels
	Model    string
	At       time.Time
	Key      string // the task key for the poll
	Outdated bool   // the position's size or entry changed since
	Auto     string // what new positions get without a click: "off", "suggest", "plan"
}

// positionSuggestion loads the sheet's view of the suggestion.
func (h *Handlers) positionSuggestion(ctx context.Context, pos positionView) positionAIView {
	v := positionAIView{Key: "levels:" + strconv.FormatInt(pos.AccountID, 10) + ":" + pos.Symbol}
	if h.AITasks == nil || pos.Opened.ID == 0 {
		return v
	}
	uid := userID(ctx)
	key := aiTaskKey(uid, "levels", strconv.FormatInt(pos.AccountID, 10), pos.Symbol)
	v.Pending = h.AITasks.Pending(key)
	v.Error = h.AITasks.TakeError(uid, key)
	v.Auto = h.userAutoPlan(ctx)
	out, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindPositionLevels, Ref: aiLevelsRef(pos.AccountID, pos.Symbol, pos.Opened.ID)})
	if err != nil {
		return v
	}
	if err := json.Unmarshal([]byte(out.Data), &v.Levels); err != nil {
		return v
	}
	v.Has, v.Model, v.At = true, out.Model, out.CreatedAt
	if pos.AvgEntry != nil && (!sameFloat(pos.AvgEntry, &v.Levels.Entry) || pos.Qty != v.Levels.Qty) {
		v.Outdated = true
	}
	return v
}

// tradeSuggestion is the frozen suggestion of the position a closed trade
// came from, nil when none was made.
func (h *Handlers) tradeSuggestion(ctx context.Context, trade models.GetTradeByIDRow) *positionAIView {
	execID, err := h.rq().FirstEntryExecutionForTrade(ctx, trade.ID)
	if err != nil {
		return nil
	}
	out, err := h.rq().GetAIOutput(ctx, models.GetAIOutputParams{UserID: userID(ctx), Kind: aiKindPositionLevels, Ref: aiLevelsRef(trade.AccountID, trade.Symbol, execID)})
	if err != nil {
		return nil
	}
	v := &positionAIView{Has: true, Model: out.Model, At: out.CreatedAt}
	if err := json.Unmarshal([]byte(out.Data), &v.Levels); err != nil {
		return nil
	}
	return v
}

// PositionSuggestStart: POST /positions/{account}/{symbol}/suggest (htmx) -
// ask the model for a stop and target on the open position, in the
// background; the sheet polls and redraws.
func (h *Handlers) PositionSuggestStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.AITasks == nil || !h.aiAvailable(ctx) {
		http.Error(w, errNoAIProvider.Error(), http.StatusConflict)
		return
	}
	pos, ok := h.loadPosition(w, r)
	if !ok {
		return
	}
	if pos.Qty == 0 || pos.AvgEntry == nil {
		http.Error(w, "the position is flat", http.StatusConflict)
		return
	}
	accountID, symbol := pos.AccountID, pos.Symbol
	h.startTask(ctx, aiTaskKey(userID(ctx), "levels", strconv.FormatInt(accountID, 10), symbol), func(ctx context.Context) error {
		return h.suggestPositionLevels(ctx, accountID, symbol)
	})
	if r.Header.Get("HX-Request") == "true" {
		if pos, ok := h.loadPosition(w, r); ok {
			h.renderPositionSheet(w, r, pos, "")
		}
		return
	}
	http.Redirect(w, r, positionHref(accountID, symbol), http.StatusSeeOther)
}

// PositionSuggestApply: POST /positions/{account}/{symbol}/suggest/apply -
// write the suggested stop and first target into the plan (source 'ai'),
// keeping a thesis the trader typed. On a leg of a copied decision the
// plan goes on every leg, like the sheet's form.
func (h *Handlers) PositionSuggestApply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pos, ok := h.loadPosition(w, r)
	if !ok {
		return
	}
	v := h.positionSuggestion(ctx, pos)
	if !v.Has || pos.Qty == 0 || pos.AvgEntry == nil {
		http.Error(w, "no suggestion to apply", http.StatusConflict)
		return
	}
	msg, err := h.applyPositionSuggestion(ctx, pos, v)
	if err != nil {
		h.serverError(w, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Trigger", `{"toast":{"message":`+strconv.Quote(msg+" from the AI suggestion")+`,"kind":"success"}}`)
		if pos, ok := h.loadPosition(w, r); ok {
			h.renderPositionSheet(w, r, pos, "plan")
		}
		return
	}
	http.Redirect(w, r, positionHref(pos.AccountID, pos.Symbol), http.StatusSeeOther)
}

// applyPositionSuggestion writes the suggestion's stop and first target
// into the position's plan (source 'ai'), on every leg of a copied
// decision, keeping a thesis the trader typed; the message is the sheet's.
func (h *Handlers) applyPositionSuggestion(ctx context.Context, pos positionView, v positionAIView) (string, error) {
	stop, target := v.Levels.Stop, v.Levels.Target
	thesis := pos.Thesis
	if thesis == "" {
		thesis = aiTruncate(v.Levels.Summary, 300)
	}
	return h.savePositionPlan(ctx, pos, positionPlanInput{Stop: &stop, Target: &target, Thesis: thesis, AllLegs: pos.InSet(), Source: "ai"})
}

// PositionSuggestDismiss: POST /positions/{account}/{symbol}/suggest/dismiss.
func (h *Handlers) PositionSuggestDismiss(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pos, ok := h.loadPosition(w, r)
	if !ok {
		return
	}
	if pos.Opened.ID != 0 {
		_ = h.Queries.DeleteAIOutput(ctx, models.DeleteAIOutputParams{UserID: userID(ctx), Kind: aiKindPositionLevels, Ref: aiLevelsRef(pos.AccountID, pos.Symbol, pos.Opened.ID)})
	}
	if r.Header.Get("HX-Request") == "true" {
		h.renderPositionSheet(w, r, pos, "")
		return
	}
	http.Redirect(w, r, positionHref(pos.AccountID, pos.Symbol), http.StatusSeeOther)
}
