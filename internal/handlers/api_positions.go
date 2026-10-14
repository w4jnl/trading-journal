package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- Open positions over the API and MCP --------------------------------
//
// GET /api/v1/positions lists what is open with the plan on each position;
// POST /api/v1/positions/{account}/{symbol}/plan writes a plan with source
// 'api', which the sheet shows dashed until the trader saves it (the same
// contract as a tag or label written through the API). The MCP tools
// list_positions and set_position_plan are the same calls.

type apiPosition struct {
	AccountID   int64      `json:"accountId"`
	Account     string     `json:"account"`
	Firm        *string    `json:"firm"`
	Symbol      string     `json:"symbol"`
	SymbolRoot  string     `json:"symbolRoot"`
	Direction   string     `json:"direction"`
	Qty         float64    `json:"qty"`
	AvgEntry    *float64   `json:"avgEntry"`
	OpenedAt    *time.Time `json:"openedAt"`
	Fills       int        `json:"fills"`
	LastPrice   *float64   `json:"lastPrice"`
	LastAt      *time.Time `json:"lastAt"`
	PriceSource string     `json:"priceSource,omitempty"`
	Unrealized  *float64   `json:"unrealized"`
	// The plan: null fields when none recorded. source says who set it
	// ('trader', 'api', 'ai', 'set' for one inherited from the copied
	// decision); setPlan whether it was saved for every leg.
	Stop        *float64 `json:"stop"`
	Risk        *float64 `json:"risk"`
	Target      *float64 `json:"target"`
	Thesis      string   `json:"thesis,omitempty"`
	PlanSource  string   `json:"planSource,omitempty"`
	SetPlan     bool     `json:"setPlan"`
	TicksToStop *float64 `json:"ticksToStop"`
	BeyondStop  bool     `json:"beyondStop"`
	// Legs of the same copied decision, as account ids; empty for a
	// position on its own.
	SetLegs []int64 `json:"setLegs"`
	Stale   bool    `json:"stale"`
	Plan    string  `json:"planUrl"`
}

func apiPositionFrom(v positionView) apiPosition {
	p := apiPosition{
		AccountID: v.AccountID, Account: v.AccountName, Firm: v.Firm, Symbol: v.Symbol, SymbolRoot: v.SymbolRoot,
		Direction: v.Direction, Qty: v.Qty, AvgEntry: v.AvgEntry, Fills: v.Fills,
		LastPrice: v.LastPrice, LastAt: v.LastAt, PriceSource: v.PriceSource, Unrealized: v.Unrealized,
		Stop: v.Stop, Risk: v.Risk, Target: v.Target, Thesis: v.Thesis, PlanSource: v.PlanSource, SetPlan: v.SetPlan,
		TicksToStop: v.TicksToStop, BeyondStop: v.BeyondStop, Stale: v.Stale,
		SetLegs: []int64{},
		Plan:    "/api/v1/positions/" + strconv.FormatInt(v.AccountID, 10) + "/" + v.Symbol + "/plan",
	}
	if !v.OpenedAt.IsZero() {
		t := v.OpenedAt
		p.OpenedAt = &t
	}
	for _, l := range v.Legs {
		p.SetLegs = append(p.SetLegs, l.AccountID)
	}
	return p
}

// openPositionsAPI lists the caller's open positions as API rows.
func (h *Handlers) openPositionsAPI(ctx context.Context) ([]apiPosition, error) {
	rows, err := h.rq().ListOpenPositions(ctx, userID(ctx))
	if err != nil {
		return nil, err
	}
	views := h.openPositionRows(ctx, rows)
	out := make([]apiPosition, 0, len(views))
	for _, v := range views {
		out = append(out, apiPositionFrom(v))
	}
	return out, nil
}

// APIPositionsList: GET /api/v1/positions - every open position of the
// caller with its plan. ?refresh=1 pulls the latest broker fills first.
func (h *Handlers) APIPositionsList(w http.ResponseWriter, r *http.Request) {
	h.syncForAPIRead(r.Context(), apiWantsRefresh(r))
	items, err := h.openPositionsAPI(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not list positions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// positionPlanBody is the JSON body of a plan write: null or absent fields
// are cleared (the whole plan is replaced, like the sheet's form).
type positionPlanBody struct {
	Stop    *float64 `json:"stop"`
	Risk    *float64 `json:"risk"`
	Target  *float64 `json:"target"`
	Thesis  string   `json:"thesis"`
	AllLegs *bool    `json:"allLegs"`
}

// applyPositionPlan validates and writes a plan on the caller's open
// position with the given source. The account must be the caller's and the
// position open; errors carry a status.
func (h *Handlers) applyPositionPlan(ctx context.Context, accountID int64, symbol string, in positionPlanBody, source string) (apiPosition, int, error) {
	if _, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: accountID, UserID: userID(ctx)}); err != nil {
		return apiPosition{}, http.StatusNotFound, errors.New("account not found")
	}
	for _, v := range []*float64{in.Stop, in.Risk, in.Target} {
		if v != nil && *v <= 0 {
			return apiPosition{}, http.StatusBadRequest, errors.New("stop, risk and target must be positive numbers")
		}
	}
	in.Thesis = strings.TrimSpace(in.Thesis)
	if len(in.Thesis) > 2000 {
		return apiPosition{}, http.StatusBadRequest, errors.New("thesis is too long (2000 characters)")
	}
	rows, err := h.rq().ListOpenPositions(ctx, userID(ctx))
	if err != nil {
		return apiPosition{}, http.StatusInternalServerError, err
	}
	var pos *positionView
	views := h.openPositionRows(ctx, rows)
	for i := range views {
		if views[i].AccountID == accountID && views[i].Symbol == symbol {
			pos = &views[i]
		}
	}
	if pos == nil || pos.Qty == 0 || pos.AvgEntry == nil {
		return apiPosition{}, http.StatusConflict, errors.New("no open position on this account and contract")
	}
	allLegs := pos.InSet() && (in.AllLegs == nil || *in.AllLegs)
	if _, err := h.savePositionPlan(ctx, *pos, positionPlanInput{Stop: in.Stop, Risk: in.Risk, Target: in.Target, Thesis: in.Thesis, AllLegs: allLegs, Source: source}); err != nil {
		return apiPosition{}, http.StatusInternalServerError, err
	}
	items, err := h.openPositionsAPI(ctx)
	if err != nil {
		return apiPosition{}, http.StatusInternalServerError, err
	}
	for _, it := range items {
		if it.AccountID == accountID && it.Symbol == symbol {
			return it, http.StatusOK, nil
		}
	}
	return apiPosition{}, http.StatusInternalServerError, errors.New("position vanished")
}

// APIPositionPlanSave: POST /api/v1/positions/{account}/{symbol}/plan.
func (h *Handlers) APIPositionPlanSave(w http.ResponseWriter, r *http.Request) {
	accountID, err := strconv.ParseInt(r.PathValue("account"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad account id")
		return
	}
	var in positionPlanBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	item, code, err := h.applyPositionPlan(r.Context(), accountID, r.PathValue("symbol"), in, "api")
	if err != nil {
		if code >= 500 {
			h.logger().Error("api position plan", "err", err)
			writeJSONError(w, code, "could not save the plan")
			return
		}
		writeJSONError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"position": item})
}

func (h *Handlers) mcpListPositions(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	h.syncForAPIRead(ctx, req.GetBool("refresh", false))
	items, err := h.openPositionsAPI(ctx)
	if err != nil {
		return mcpgo.NewToolResultError("could not list positions"), nil
	}
	return mcpJSON(map[string]any{"items": items})
}

func (h *Handlers) mcpSetPositionPlan(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	accountID, err := req.RequireInt("accountId")
	if err != nil {
		return mcpgo.NewToolResultError("accountId is required"), nil
	}
	symbol, err := req.RequireString("symbol")
	if err != nil {
		return mcpgo.NewToolResultError("symbol is required"), nil
	}
	args := req.GetArguments()
	num := func(k string) *float64 {
		if v, ok := args[k].(float64); ok && v > 0 {
			return &v
		}
		return nil
	}
	in := positionPlanBody{Stop: num("stop"), Risk: num("risk"), Target: num("target"), Thesis: req.GetString("thesis", "")}
	if v, ok := args["allLegs"].(bool); ok {
		in.AllLegs = &v
	}
	item, _, err := h.applyPositionPlan(ctx, int64(accountID), symbol, in, "api")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpJSON(map[string]any{"position": item})
}
