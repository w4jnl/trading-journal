package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/coach"
	"github.com/w4jnl/trading-journal/internal/journal"
)

// Coach checks (internal/coach) on the day page, in prepare-day's result,
// the API and MCP.

// coachReport runs the checks for a trading-timezone day; a nil report
// means they could not run (logged), never a 500 on a journal page.
func (h *Handlers) coachReport(ctx context.Context, day time.Time) *coach.Report {
	r, err := coach.Run(ctx, h.rdb(), userID(ctx), day, h.localTZ())
	if err != nil {
		h.logger().Warn("coach checks", "day", day.Format("2006-01-02"), "err", err)
		return nil
	}
	return r
}

// prepDayResponse is prepare-day's result with the day's checks attached
// (the journal service stays unaware of the coach).
type prepDayResponse struct {
	*journal.PrepResult
	Checks *coach.Report `json:"checks,omitempty"`
}

// coachDay resolves a day argument the way the stats endpoints do: a date
// or today|yesterday|last; empty means the last trading day.
func (h *Handlers) coachDay(ctx context.Context, arg string) (time.Time, error) {
	tz := h.localTZ()
	if arg == "" {
		arg = "last"
	}
	v := h.resolveDayKeyword(ctx, arg, tz)
	d, err := time.ParseInLocation("2006-01-02", v, tz)
	if err != nil {
		return time.Time{}, journal.BadInput("bad day; expected YYYY-MM-DD or today|yesterday|last")
	}
	return d, nil
}

// APICoachChecks: GET /api/v1/coach?day=
func (h *Handlers) APICoachChecks(w http.ResponseWriter, r *http.Request) {
	day, err := h.coachDay(r.Context(), r.URL.Query().Get("day"))
	if err != nil {
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "could not resolve the day")
		return
	}
	rep, err := coach.Run(r.Context(), h.rdb(), userID(r.Context()), day, h.localTZ())
	if err != nil {
		h.logger().Warn("api coach", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not run the checks")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (h *Handlers) mcpGetCoachChecks(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	day, err := h.coachDay(ctx, req.GetString("day", ""))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	rep, err := coach.Run(ctx, h.rdb(), userID(ctx), day, h.localTZ())
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("could not run the checks: %v", err)), nil
	}
	return mcpJSON(rep)
}

// coachFor keeps the findings that touch the given trades (an entry's
// trades); nil ids keeps every finding (the day).
func coachFor(rep *coach.Report, ids []int64) *coach.Report {
	if rep == nil || ids == nil {
		return rep
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := *rep
	out.Findings = nil
	for _, f := range rep.Findings {
		for _, id := range f.TradeIDs {
			if want[id] {
				out.Findings = append(out.Findings, f)
				break
			}
		}
	}
	return &out
}
