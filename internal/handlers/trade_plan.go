package handlers

import (
	"fmt"
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
	if risk == nil && stop != nil {
		if inst, err := h.Queries.GetInstrument(ctx, trade.SymbolRoot); err == nil {
			risk = positions.PlannedRisk(trade.EntryPrice, *stop, trade.Qty, inst)
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
