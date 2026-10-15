package handlers

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- Automatic suggestions and plans on new positions -----------------------
//
// Settings > AI chooses what a new open position gets (users.auto_plan):
// nothing (the suggestion stays a button on the sheet), the suggestion
// made for it and waiting on the sheet (autoPlanSuggest), or the
// suggestion written straight into the plan with source 'ai'
// (autoPlanSet). The planner looks at a user's open positions whenever
// their data moves (a fill arrived, a plan changed) and runs, for each
// cycle not judged yet, the suggestion the sheet's button runs; a copied
// decision is one run from its earliest leg, and a plan goes on every leg
// at once. A cycle is judged once (the stored suggestion is the record of
// that), so a plan the trader cleared stays cleared and a suggestion they
// dismissed is not made again; a failed attempt waits autoPlanRetry before
// the next. Carried positions (open for days) are left alone, as
// everywhere else. In the plan mode a cycle the trader planned already is
// left to them; in the suggest mode every new cycle gets its suggestion.
// A scale-in (a fill adding to an open position) is the same cycle: the
// plan set on the first fill is never touched, no second suggestion is
// made, only the money at risk follows the size (fillPositionView).

// The users.auto_plan modes.
const (
	autoPlanOff     = 0
	autoPlanSuggest = 1
	autoPlanSet     = 2
)

// autoPlanModeName is the mode as the settings form and the views name it.
func autoPlanModeName(mode int64) string {
	switch mode {
	case autoPlanSuggest:
		return "suggest"
	case autoPlanSet:
		return "plan"
	}
	return "off"
}

// autoPlanModeOf is the inverse, false for an unknown name.
func autoPlanModeOf(name string) (int64, bool) {
	switch name {
	case "off":
		return autoPlanOff, true
	case "suggest":
		return autoPlanSuggest, true
	case "plan":
		return autoPlanSet, true
	}
	return 0, false
}

const (
	// autoPlanEvery is how often the users with the setting are looked at.
	autoPlanEvery = 15 * time.Second
	// autoPlanRetry is the wait after a failed suggestion (model down, an
	// answer the checks refused) before the same cycle is tried again.
	autoPlanRetry = 10 * time.Minute
)

// autoPlanner is the planner's memory: the data version last read per
// user, and the last attempt per cycle.
type autoPlanner struct {
	mu    sync.Mutex
	seen  map[int64]uint64
	tried map[string]time.Time
}

// RunAutoPlanner ticks until ctx ends. main.go starts it when AI tasks run.
func (h *Handlers) RunAutoPlanner(ctx context.Context) {
	t := time.NewTicker(autoPlanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.autoPlanTick(ctx)
		}
	}
}

// autoPlanTick plans for every user with the setting whose data changed
// since the last look.
func (h *Handlers) autoPlanTick(ctx context.Context) {
	ids, err := h.rq().ListAutoPlanUserIDs(ctx)
	if err != nil {
		h.logger().Warn("auto plan: users", "err", err)
		return
	}
	for _, uid := range ids {
		v := h.LiveData.Version(uid)
		h.autoPlan.mu.Lock()
		if h.autoPlan.seen == nil {
			h.autoPlan.seen = map[int64]uint64{}
		}
		last, known := h.autoPlan.seen[uid]
		h.autoPlan.seen[uid] = v
		h.autoPlan.mu.Unlock()
		if known && last == v {
			continue
		}
		h.autoPlanUser(ctx, uid)
	}
}

// autoPlanUser starts a suggestion (and, in the plan mode, its apply) for
// each of the user's open decisions not judged yet and reports how many it
// started. The work runs as the user (their provider, their language),
// like the sheet's button.
func (h *Handlers) autoPlanUser(ctx context.Context, uid int64) int {
	u, err := h.rq().GetUserByID(ctx, uid)
	if err != nil || u.AutoPlan == autoPlanOff || u.IsActive == 0 {
		return 0
	}
	apply := u.AutoPlan == autoPlanSet
	ctx = auth.WithUser(ctx, &auth.User{ID: u.ID, Username: u.Username, DisplayName: derefStr(u.DisplayName), Email: derefStr(u.Email), IsAdmin: u.IsAdmin != 0})
	if h.AITasks == nil || !h.aiAvailable(ctx) {
		return 0
	}
	rows, err := h.rq().ListOpenPositions(ctx, uid)
	if err != nil {
		h.logger().Warn("auto plan: open positions", "user", uid, "err", err)
		return 0
	}
	started := 0
	now := time.Now()
	for _, v := range h.openPositionRows(ctx, rows) {
		if v.AvgEntry == nil || v.Qty == 0 || v.Stale || (apply && v.HasPlan()) || !leadsDecision(v, apply) {
			continue
		}
		ref := aiLevelsRef(v.AccountID, v.Symbol, v.Opened.ID)
		if _, err := h.rq().GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindPositionLevels, Ref: ref}); err == nil {
			continue // judged once already: the trader cleared or ignored it
		}
		key := aiTaskKey(uid, "levels", strconv.FormatInt(v.AccountID, 10), v.Symbol)
		if h.AITasks.Pending(key) {
			continue
		}
		h.autoPlan.mu.Lock()
		if h.autoPlan.tried == nil {
			h.autoPlan.tried = map[string]time.Time{}
		}
		last, tried := h.autoPlan.tried[ref]
		if tried && now.Sub(last) < autoPlanRetry {
			h.autoPlan.mu.Unlock()
			continue
		}
		h.autoPlan.tried[ref] = now
		h.autoPlan.mu.Unlock()
		accountID, symbol := v.AccountID, v.Symbol
		h.startTask(ctx, key, func(ctx context.Context) error {
			return h.autoPlanPosition(ctx, accountID, symbol, apply)
		})
		started++
	}
	return started
}

// leadsDecision reports whether the leg is the one to judge its decision
// from: a position on its own, or the earliest leg of a set (the one the
// guardrail keys on too). When a plan is to be written, a set with a plan
// on another leg is left to inheritance.
func leadsDecision(v positionView, unplanned bool) bool {
	for _, l := range v.Legs {
		if unplanned && l.HasPlan() {
			return false
		}
		if l.OpenedAt.Before(v.OpenedAt) || (l.OpenedAt.Equal(v.OpenedAt) && l.AccountID < v.AccountID) {
			return false
		}
	}
	return true
}

// autoPlanPosition runs the suggestion and, when apply is set, writes it
// into the plan, unless the position closed or got a plan from the trader
// meanwhile. The suggestion alone bumps the user's version so an open
// sheet shows it.
func (h *Handlers) autoPlanPosition(ctx context.Context, accountID int64, symbol string, apply bool) error {
	if err := h.suggestPositionLevels(ctx, accountID, symbol); err != nil {
		return err
	}
	if !apply {
		h.LiveData.Bump(userID(ctx))
		return nil
	}
	pos, ok := h.openPosition(ctx, accountID, symbol)
	if !ok || pos.HasPlan() {
		return nil
	}
	v := h.positionSuggestion(ctx, pos)
	if !v.Has || v.Outdated {
		return nil
	}
	msg, err := h.applyPositionSuggestion(ctx, pos, v)
	if err != nil {
		return err
	}
	h.logger().Info("auto plan set", "user", userID(ctx), "account", accountID, "symbol", symbol,
		"stop", v.Levels.Stop, "target", v.Levels.Target, "legs", len(pos.Legs)+1, "msg", msg)
	return nil
}

// userAutoPlan is the caller's mode by name: "off", "suggest" or "plan".
func (h *Handlers) userAutoPlan(ctx context.Context) string {
	u, err := h.rq().GetUserByID(ctx, userID(ctx))
	if err != nil {
		return "off"
	}
	return autoPlanModeName(u.AutoPlan)
}

// derefStr reads an optional string.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
