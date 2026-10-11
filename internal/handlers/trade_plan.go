package handlers

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// TradePlanSave handles POST /trades/{id}/plan: the trade sheet's Plan row.
// Form fields: stop (price) and risk (money); an empty form clears the plan.
// The risk follows the stop through the instrument's tick specs unless the
// trader typed one, so "stop at 7840" is enough on a configured instrument
// and a stub instrument still records the stop for later. R = net / risk
// then shows on the sheet, the trades table, the API and analytics.
func (h *Handlers) TradePlanSave(w http.ResponseWriter, r *http.Request) {
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
	trade, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		http.Error(w, "trade not found", http.StatusNotFound)
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
	// Either side derives the other through the instrument's point value:
	// a stop sets the risk, a risk sets the stop (on the losing side of the
	// entry, snapped to the tick). The form always posts both fields, so
	// when both arrive the one that differs from the stored plan is the one
	// the trader edited and the other follows it; both changed means both
	// were typed on purpose and both are kept.
	derivedStop := false
	if inst, err := h.Queries.GetInstrument(ctx, trade.SymbolRoot); err == nil {
		stopEdited := !sameFloat(stop, trade.PlannedStop)
		riskEdited := !sameFloat(risk, trade.PlannedRisk)
		switch {
		case stop != nil && (risk == nil || (stopEdited && !riskEdited)):
			if r := positions.PlannedRisk(trade.EntryPrice, *stop, trade.Qty, inst); r != nil || risk == nil {
				risk = r
			}
		case risk != nil && (stop == nil || (riskEdited && !stopEdited)):
			if st := positions.PlannedStop(trade.EntryPrice, *risk, trade.Qty, trade.Direction, inst); st != nil || stop == nil {
				stop = st
				derivedStop = st != nil
			}
		}
	}
	n, err := h.Queries.SetTradePlan(ctx, models.SetTradePlanParams{PlannedStop: stop, PlannedRisk: risk, ID: id, UserID: userID(ctx)})
	if err != nil {
		h.serverError(w, fmt.Errorf("set trade plan: %w", err))
		return
	}
	if n == 0 {
		http.NotFound(w, r)
		return
	}
	h.LiveData.Bump(userID(ctx))

	msg := "Plan saved"
	switch {
	case stop == nil && risk == nil:
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
		h.renderTradeDetail(w, r, "plan")
		return
	}
	q := url.Values{}
	q.Set("flash", "plan")
	if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		q.Set("back", v)
	}
	http.Redirect(w, r, fmt.Sprintf("/journal/trade/%d?%s", id, q.Encode()), http.StatusSeeOther)
}

// sameFloat reports two optional numbers equal (both nil, or within a
// rounding hair: the form re-renders stored values with %g / %.2f).
func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) < 0.005
}

// parsePositiveFloat reads an optional form number: "" -> nil, ok; a
// positive number -> value, ok; anything else -> not ok.
func parsePositiveFloat(s string) (*float64, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return nil, true
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return nil, false
	}
	return &v, true
}
