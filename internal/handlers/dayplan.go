package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/coach"
	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/journal"
)

// The pre-market plan (internal/dayplan): written on the day page (any
// day; one written after the first trade is flagged), through the API and
// MCP (today or a future day only), read against the day by the coach.

// dayPlan loads the user's plan for a day (nil without one; errors logged).
func (h *Handlers) dayPlan(ctx context.Context, day string) *dayplan.Plan {
	p, err := dayplan.Get(ctx, h.Queries, userID(ctx), day)
	if err != nil {
		h.logger().Warn("day plan", "day", day, "err", err)
		return nil
	}
	return p
}

// DayPlanSave handles POST /journal/plan/{date} from the day page's Plan
// card. An empty form (or _action=delete) removes the plan.
func (h *Handlers) DayPlanSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	day := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	target := "/journal/" + day
	if back := r.FormValue("back"); back != "" && strings.HasPrefix(back, "/") && !strings.HasPrefix(back, "//") {
		target += "?back=" + url.QueryEscape(back)
	}
	target += "#day-plan"
	p := dayplan.Plan{Day: day}
	if r.FormValue("_action") != "delete" {
		maxTrades, err := dayplan.ParseInt(r.FormValue("max_trades"))
		if err == nil {
			p.MaxTrades = maxTrades
			p.MaxLoss, err = dayplan.ParseFloat(r.FormValue("max_loss"))
		}
		if err != nil {
			h.planFailed(w, err)
			return
		}
		p.Bias, p.Levels, p.StopTime, p.Notes = r.FormValue("bias"), r.FormValue("levels"), r.FormValue("stop_time"), r.FormValue("notes")
	}
	if _, err := dayplan.Save(ctx, h.Queries, userID(ctx), p, time.Now()); err != nil {
		h.planFailed(w, err)
		return
	}
	h.dropPlanDraft(ctx, day) // saved or deleted: the draft has served
	h.LiveData.Bump(userID(ctx))
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handlers) planFailed(w http.ResponseWriter, err error) {
	var ve apiValidationError
	if errors.As(err, &ve) {
		http.Error(w, ve.Msg, http.StatusBadRequest)
		return
	}
	h.serverError(w, err)
}

// planPatch is a partial plan: a field that is present replaces the stored
// one (an empty string or zero clears it), an absent field is kept.
type planPatch struct {
	Day       string   `json:"day"`
	Bias      *string  `json:"bias"`
	Levels    *string  `json:"levels"`
	MaxTrades *int64   `json:"maxTrades"`
	StopTime  *string  `json:"stopTime"`
	MaxLoss   *float64 `json:"maxLoss"`
	Notes     *string  `json:"notes"`
}

// applyPlanPatch merges a patch onto the day's plan for the API and MCP,
// which may write today or a future day only (the record of a past day is
// edited on the page, where a late edit is flagged).
func (h *Handlers) applyPlanPatch(ctx context.Context, in planPatch) (*dayplan.Plan, error) {
	tz := h.localTZ()
	day := h.resolveDayKeyword(ctx, strings.TrimSpace(in.Day), tz)
	if strings.TrimSpace(in.Day) == "" {
		day = todayLocal(tz).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return nil, journal.BadInput("bad day; expected YYYY-MM-DD or today")
	}
	if day < todayLocal(tz).Format("2006-01-02") {
		return nil, journal.BadInput("a plan can be written for today or a future day; past plans are edited on the day's journal page")
	}
	p := dayplan.Plan{Day: day}
	if cur := h.dayPlan(ctx, day); cur != nil {
		p = *cur
	}
	if in.Bias != nil {
		p.Bias = *in.Bias
	}
	if in.Levels != nil {
		p.Levels = *in.Levels
	}
	if in.MaxTrades != nil {
		p.MaxTrades = in.MaxTrades
	}
	if in.StopTime != nil {
		p.StopTime = *in.StopTime
	}
	if in.MaxLoss != nil {
		p.MaxLoss = in.MaxLoss
	}
	if in.Notes != nil {
		p.Notes = *in.Notes
	}
	saved, err := dayplan.Save(ctx, h.Queries, userID(ctx), p, time.Now())
	if err != nil {
		return nil, err
	}
	h.LiveData.Bump(userID(ctx))
	return saved, nil
}

// dayPlanView is the plan of a day with its outcome so far.
type dayPlanView struct {
	Day     string             `json:"day"`
	Plan    *dayplan.Plan      `json:"plan"`
	Outcome *coach.PlanOutcome `json:"outcome"`
}

func (h *Handlers) dayPlanView(ctx context.Context, dayArg string) (dayPlanView, error) {
	tz := h.localTZ()
	if strings.TrimSpace(dayArg) == "" {
		dayArg = "today"
	}
	v := h.resolveDayKeyword(ctx, dayArg, tz)
	d, err := time.ParseInLocation("2006-01-02", v, tz)
	if err != nil {
		return dayPlanView{}, journal.BadInput("bad day; expected YYYY-MM-DD or today|yesterday|last")
	}
	out := dayPlanView{Day: v, Plan: h.dayPlan(ctx, v)}
	if out.Plan != nil {
		if rep := h.coachReport(ctx, d); rep != nil {
			out.Outcome = rep.Plan
		}
	}
	return out, nil
}

// APIDayPlan: GET /api/v1/plan?day=   (default today)
func (h *Handlers) APIDayPlan(w http.ResponseWriter, r *http.Request) {
	v, err := h.dayPlanView(r.Context(), r.URL.Query().Get("day"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// APIDayPlanSave: POST /api/v1/plan {day?, bias?, levels?, maxTrades?, stopTime?, maxLoss?, notes?}
func (h *Handlers) APIDayPlanSave(w http.ResponseWriter, r *http.Request) {
	var in planPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p, err := h.applyPlanPatch(r.Context(), in)
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "could not save the plan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": p})
}

func (h *Handlers) mcpGetDayPlan(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	v, err := h.dayPlanView(ctx, req.GetString("day", ""))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpJSON(v)
}

func (h *Handlers) mcpSetDayPlan(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := req.GetArguments()
	in := planPatch{Day: req.GetString("day", "")}
	str := func(k string) *string {
		if v, ok := args[k].(string); ok {
			return &v
		}
		return nil
	}
	in.Bias, in.Levels, in.StopTime, in.Notes = str("bias"), str("levels"), str("stopTime"), str("notes")
	if v, ok := args["maxTrades"]; ok {
		if n, ok := toInt64(v); ok {
			in.MaxTrades = &n
		}
	}
	if v, ok := args["maxLoss"].(float64); ok {
		in.MaxLoss = &v
	}
	p, err := h.applyPlanPatch(ctx, in)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if p == nil {
		return mcpJSON(map[string]any{"plan": nil, "message": "the plan is empty and was removed"})
	}
	return mcpJSON(map[string]any{"plan": p})
}

// planSummary is the one-line form of a plan for the dashboard.
func planSummary(p *dayplan.Plan) string {
	if p == nil {
		return ""
	}
	var parts []string
	if p.Bias != "" {
		parts = append(parts, dayplan.BiasName(p.Bias))
	}
	if p.MaxTrades != nil {
		parts = append(parts, fmt.Sprintf("max %d", *p.MaxTrades))
	}
	if p.StopTime != "" {
		parts = append(parts, "done by "+p.StopTime)
	}
	if p.MaxLoss != nil {
		parts = append(parts, fmt.Sprintf("stop at −%.0f", *p.MaxLoss))
	}
	if len(parts) == 0 {
		return "levels and notes"
	}
	return strings.Join(parts, " · ")
}
