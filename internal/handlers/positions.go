package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// ---- Open positions: the position as a row and a sheet, with its plan ----
//
// A position is open while an account's fills on one contract do not net
// flat (ListOpenPositions). It has no trade row yet - the builder writes
// closed cycles only - so its plan (stop, risk, target, thesis) lives in
// position_plans, anchored on the fill that opened the cycle, and follows
// the cycle into the trade when it closes (positions.HandOffPlan). The sheet
// for a position opens in the trade sheet under the key "pos:<account>:<symbol>"
// (trade_sheet.js), from the /trades open strip, the dashboard card and the
// nav badge.

// positionStaleAfter is how old an open cycle may be before the views call
// it out: a futures position held for days is usually a carried or data-gap
// position (fills missing), not a plan to make.
const positionStaleAfter = 3 * 24 * time.Hour

// positionView is one open position with its live numbers and plan. AvgEntry,
// LastPrice and Unrealized are pointers because the partials render "—" when
// the fills could not be replayed, no provider priced the contract, or the
// instrument has no tick specs.
type positionView struct {
	models.ListOpenPositionsRow
	Href      string // where a click goes: the sheet, deep-linked on /trades
	Title     string
	Key       string // "pos:<account>:<symbol>", the trade sheet's row key
	SheetURL  string // GET of the sheet partial
	PlanURL   string // POST of the plan form
	Direction string // "long" | "short"
	Qty       float64
	AvgEntry  *float64
	OpenedAt  time.Time
	Fills     int
	Opened    models.Execution
	Stale     bool

	LastPrice   *float64
	LastAt      *time.Time
	PriceSource string
	Unrealized  *float64

	// The plan, nil when none recorded for this cycle. Risk is kept current
	// with the position's size (a scale-in after the plan was set changes the
	// money at risk, not the stop).
	Plan        *models.PositionPlan
	Stop        *float64
	Risk        *float64
	Target      *float64
	Thesis      string
	PlanSource  string
	TicksToStop *float64 // distance from the last price to the stop, in ticks (negative: through it)
	BeyondStop  bool     // the last price is through the stop
	RewardRisk  string   // "2.1" when stop and target are set, "" otherwise
	PointValue  float64  // for the form's live mirror (0 on a stub instrument)
	TickSize    float64

	// Fills of the cycle (entries and partial exits), for the sheet.
	CycleFills []models.Execution
	// Admin link to the reconcile editor for this account and contract.
	ReconcileHref string
}

// HasPlan reports whether any level is recorded.
func (p positionView) HasPlan() bool { return p.Stop != nil || p.Risk != nil || p.Target != nil }

// positionKey is the trade sheet's row key for an open position.
func positionKey(accountID int64, symbol string) string {
	return "pos:" + strconv.FormatInt(accountID, 10) + ":" + symbol
}

// positionSheetURL is the sheet partial of an open position.
func positionSheetURL(accountID int64, symbol string) string {
	return "/positions/" + strconv.FormatInt(accountID, 10) + "/" + url.PathEscape(symbol)
}

// positionHref is the full-page link to a position: /trades narrowed to open
// positions with the sheet deep-linked (trade_sheet.js opens ?sheet=).
func positionHref(accountID int64, symbol string) string {
	q := url.Values{"status": {"open"}, "sheet": {positionKey(accountID, symbol)}}
	return "/trades?" + q.Encode()
}

// openPositionRows resolves the open positions of ListOpenPositions into views
// for the open_positions partials (dashboard, /admin/transactions, the /trades
// open strip): the FIFO average entry and the cycle's opening fill
// (positions.OpenFromFills), the plan anchored on that fill, the freshest
// price a provider gives and the pending P&L at it. Nil-safe on h.Queries /
// h.MarketData / h.TopstepxSync so the partial's callers and tests need no
// DB or vendor; a position whose fills cannot be loaded keeps its link only.
func (h *Handlers) openPositionRows(ctx context.Context, rows []models.ListOpenPositionsRow) []positionView {
	insts := map[string]*models.Instrument{}
	out := make([]positionView, 0, len(rows))
	for _, r := range rows {
		row := positionView{ListOpenPositionsRow: r}
		row.Key = positionKey(r.AccountID, r.Symbol)
		row.SheetURL = positionSheetURL(r.AccountID, r.Symbol)
		row.PlanURL = row.SheetURL + "/plan"
		row.Href = positionHref(r.AccountID, r.Symbol)
		row.Title = "Open " + r.Symbol + " position on " + r.AccountName + ": plan, stop and target"
		row.Direction, row.Qty = "long", r.NetQty
		if r.NetQty < 0 {
			row.Direction, row.Qty = "short", -r.NetQty
		}
		if u := auth.UserFrom(ctx); u != nil && u.IsAdmin {
			q := url.Values{"account_id": {strconv.FormatInt(r.AccountID, 10)}, "symbol": {r.Symbol}}
			row.ReconcileHref = "/admin/transactions?" + q.Encode()
		}
		if h != nil && h.Queries != nil {
			h.fillPositionView(ctx, &row, insts)
		}
		out = append(out, row)
	}
	return out
}

// fillPositionView replays the fills of row's account and contract and
// prices the result: average entry, opening fill, plan, quote, unrealized
// and the plan's live distances. Errors are logged, never fatal: the row
// stays a link.
func (h *Handlers) fillPositionView(ctx context.Context, row *positionView, insts map[string]*models.Instrument) {
	fills, err := h.rq().ListExecutionsForAccountSymbol(ctx, models.ListExecutionsForAccountSymbolParams{AccountID: row.AccountID, Symbol: row.Symbol})
	if err != nil {
		h.logger().Warn("open positions: load fills", "account", row.AccountID, "symbol", row.Symbol, "err", err)
		return
	}
	pos := positions.OpenFromFills(fills)
	if pos.NetQty == 0 {
		return // SQL net disagrees with the replay (data gap): no cycle to show
	}
	avg := pos.AvgPrice
	row.AvgEntry = &avg
	row.Opened, row.OpenedAt, row.Fills = pos.Opened, pos.Opened.ExecTime, pos.Fills
	row.Stale = time.Since(row.OpenedAt) > positionStaleAfter
	row.Direction, row.Qty = "long", pos.NetQty
	if pos.NetQty < 0 {
		row.Direction, row.Qty = "short", -pos.NetQty
	}
	for i, f := range fills {
		if f.ID == pos.Opened.ID {
			row.CycleFills = fills[i:]
			break
		}
	}

	inst := h.instrument(ctx, row.SymbolRoot, insts)
	if inst != nil {
		if pv, ok := positions.PointValue(*inst); ok {
			row.PointValue = pv
			if inst.TickSize != nil {
				row.TickSize = *inst.TickSize
			}
		}
	}

	plan, err := positions.FindPositionPlan(ctx, h.rq(), pos.Opened)
	switch {
	case err == nil && plan.HandedOffAt == nil:
		row.Plan = &plan
		row.Stop, row.Risk, row.Target, row.Thesis, row.PlanSource = plan.Stop, plan.Risk, plan.Target, plan.Thesis, plan.Source
		// The money at risk follows the stop at the position's current size.
		if plan.Stop != nil && inst != nil {
			if r := positions.PlannedRisk(avg, *plan.Stop, row.Qty, *inst); r != nil && !sameFloat(r, plan.Risk) {
				row.Risk = r
				if err := h.rq().SetPositionPlanRisk(ctx, models.SetPositionPlanRiskParams{Risk: r, ID: plan.ID}); err != nil {
					h.logger().Warn("open positions: update plan risk", "plan", plan.ID, "err", err)
				}
			}
		}
		if row.Stop != nil && row.Target != nil {
			row.RewardRisk = rewardRiskLabel(avg, *row.Stop, *row.Target)
		}
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		h.logger().Warn("open positions: load plan", "account", row.AccountID, "symbol", row.Symbol, "err", err)
	}

	if q := h.latestQuote(ctx, auth.UserFrom(ctx), row.Symbol, row.SymbolRoot); q != nil {
		price, at := q.Price, q.At
		row.LastPrice, row.LastAt, row.PriceSource = &price, &at, q.Source
		if inst != nil {
			row.Unrealized = unrealizedPnL(avg, q.Price, pos.NetQty, *inst)
		}
		if row.Stop != nil && row.TickSize > 0 {
			// Positive while the stop still has room, negative once price is
			// through it (below the stop of a long, above it for a short).
			d := (price - *row.Stop) / row.TickSize
			if row.Direction == "short" {
				d = -d
			}
			d = math.Round(d*10) / 10
			row.TicksToStop = &d
			row.BeyondStop = d < 0
		}
	}
}

// rewardRiskLabel is the target distance over the stop distance from the
// entry ("2.1"), "" when the stop sits on the entry.
func rewardRiskLabel(entry, stop, target float64) string {
	risk := math.Abs(entry - stop)
	if risk == 0 {
		return ""
	}
	return strconv.FormatFloat(math.Abs(target-entry)/risk, 'f', 1, 64)
}

// loadPosition resolves {account}/{symbol} of the request to the caller's
// open position. ok is false (and the response written) when the account is
// not theirs; a flat position comes back with Qty 0 so the sheet can say the
// cycle closed.
func (h *Handlers) loadPosition(w http.ResponseWriter, r *http.Request) (positionView, bool) {
	ctx := r.Context()
	accountID, err := strconv.ParseInt(r.PathValue("account"), 10, 64)
	if err != nil {
		http.Error(w, "bad account", http.StatusBadRequest)
		return positionView{}, false
	}
	symbol := r.PathValue("symbol")
	acct, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: accountID, UserID: userID(ctx)})
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return positionView{}, false
	}
	open, err := h.rq().ListOpenPositions(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list open positions: %w", err))
		return positionView{}, false
	}
	for _, row := range open {
		if row.AccountID == accountID && row.Symbol == symbol {
			views := h.openPositionRows(ctx, []models.ListOpenPositionsRow{row})
			return views[0], true
		}
	}
	// Flat: the sheet reports it and points at the trade the cycle became.
	root := symbol
	if fills, err := h.rq().ListExecutionsForAccountSymbol(ctx, models.ListExecutionsForAccountSymbolParams{AccountID: accountID, Symbol: symbol}); err == nil && len(fills) > 0 {
		root = fills[len(fills)-1].SymbolRoot
	}
	v := positionView{ListOpenPositionsRow: models.ListOpenPositionsRow{AccountID: accountID, AccountName: acct.Name, AccountRef: acct.AccountRef, Firm: acct.Firm, Symbol: symbol, SymbolRoot: root}}
	v.Key = positionKey(accountID, symbol)
	return v, true
}

// PositionSheet: GET /positions/{account}/{symbol} - the sheet of one open
// position, rendered into #trade-sheet-content like a trade.
func (h *Handlers) PositionSheet(w http.ResponseWriter, r *http.Request) {
	pos, ok := h.loadPosition(w, r)
	if !ok {
		return
	}
	h.renderPositionSheet(w, r, pos, "")
}

func (h *Handlers) renderPositionSheet(w http.ResponseWriter, r *http.Request, pos positionView, flash string) {
	ctx := r.Context()
	var closedTrade *int64
	if pos.Qty == 0 {
		// The cycle closed since the link was made: find the trade through
		// the newest handed-off plan of this account and contract, if any.
		if fills, err := h.rq().ListExecutionsForAccountSymbol(ctx, models.ListExecutionsForAccountSymbolParams{AccountID: pos.AccountID, Symbol: pos.Symbol}); err == nil {
			for i := len(fills) - 1; i >= 0 && closedTrade == nil; i-- {
				if p, err := h.rq().GetPositionPlanByExec(ctx, models.GetPositionPlanByExecParams{AccountID: pos.AccountID, OpenExecID: fills[i].ID}); err == nil && p.TradeID != nil {
					closedTrade = p.TradeID
				}
			}
		}
	}
	tradesHref := "/trades?" + url.Values{"filter": {"acc:" + strconv.FormatInt(pos.AccountID, 10)}, "symbol_root": {pos.SymbolRoot}}.Encode()
	h.renderPartial(w, "_position_sheet.html", "position_sheet", map[string]any{
		"Pos":         pos,
		"Flash":       flash,
		"ClosedTrade": closedTrade,
		"TradesHref":  tradesHref,
	})
}

// PositionPlanSave: POST /positions/{account}/{symbol}/plan - the sheet's
// plan form. Fields stop (price), risk (money), target (price), thesis
// (text); an empty form deletes the plan. Stop and risk derive each other
// through the instrument's point value at the position's current size, the
// same rule as the trade plan (TradePlanSave): the field that changed is the
// one the trader edited.
func (h *Handlers) PositionPlanSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	pos, ok := h.loadPosition(w, r)
	if !ok {
		return
	}
	if pos.Qty == 0 || pos.AvgEntry == nil {
		http.Error(w, "the position is flat: plan the trade on its sheet instead", http.StatusConflict)
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
	target, ok := parsePositiveFloat(r.FormValue("target"))
	if !ok {
		http.Error(w, "target must be a positive price", http.StatusBadRequest)
		return
	}
	thesis := strings.TrimSpace(r.FormValue("thesis"))
	if len(thesis) > 2000 {
		http.Error(w, "thesis is too long (2000 characters)", http.StatusBadRequest)
		return
	}

	derivedStop := false
	if inst, err := h.Queries.GetInstrument(ctx, pos.SymbolRoot); err == nil {
		stopEdited := !sameFloat(stop, pos.Stop)
		riskEdited := !sameFloat(risk, pos.Risk)
		switch {
		case stop != nil && (risk == nil || (stopEdited && !riskEdited)):
			if r := positions.PlannedRisk(*pos.AvgEntry, *stop, pos.Qty, inst); r != nil || risk == nil {
				risk = r
			}
		case risk != nil && (stop == nil || (riskEdited && !stopEdited)):
			if st := positions.PlannedStop(*pos.AvgEntry, *risk, pos.Qty, pos.Direction, inst); st != nil || stop == nil {
				stop = st
				derivedStop = st != nil
			}
		}
	}

	empty := stop == nil && risk == nil && target == nil && thesis == ""
	switch {
	case pos.Plan != nil && empty:
		if err := h.Queries.DeletePositionPlan(ctx, pos.Plan.ID); err != nil {
			h.serverError(w, fmt.Errorf("delete position plan: %w", err))
			return
		}
	case pos.Plan != nil:
		if err := h.Queries.UpdatePositionPlan(ctx, models.UpdatePositionPlanParams{Stop: stop, Risk: risk, Target: target, Thesis: thesis, Source: "trader", ID: pos.Plan.ID}); err != nil {
			h.serverError(w, fmt.Errorf("update position plan: %w", err))
			return
		}
	case !empty:
		if _, err := h.Queries.CreatePositionPlan(ctx, models.CreatePositionPlanParams{
			AccountID: pos.AccountID, Symbol: pos.Symbol, OpenExecID: pos.Opened.ID, OpenOrderID: pos.Opened.OrderID, OpenedAt: pos.Opened.ExecTime,
			Stop: stop, Risk: risk, Target: target, Thesis: thesis, Source: "trader",
		}); err != nil {
			h.serverError(w, fmt.Errorf("create position plan: %w", err))
			return
		}
	}
	h.LiveData.Bump(userID(ctx))

	msg := "Plan saved"
	switch {
	case empty:
		msg = "Plan cleared"
	case stop != nil && risk == nil:
		msg = "Stop saved; set the instrument's tick size and value to derive the risk"
	case stop == nil && risk != nil:
		msg = "Risk saved; set the instrument's tick size and value to derive the stop"
	case derivedStop:
		msg = fmt.Sprintf("Plan saved; stop set to %g from the risk", *stop)
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Trigger", `{"toast":{"message":`+strconv.Quote(msg)+`,"kind":"success"}}`)
		if pos, ok := h.loadPosition(w, r); ok {
			h.renderPositionSheet(w, r, pos, "plan")
		}
		return
	}
	http.Redirect(w, r, positionHref(pos.AccountID, pos.Symbol), http.StatusSeeOther)
}
