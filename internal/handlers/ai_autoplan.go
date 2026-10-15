package handlers

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- Automatic plans on new positions ---------------------------------------
//
// With "set the stop and target automatically" on (Settings > AI,
// users.auto_plan), the planner looks at a user's open positions whenever
// their data moves (a fill arrived, a plan changed) and, for every cycle
// that has no plan and no suggestion yet, runs the suggestion the sheet's
// button runs and writes it into the plan (source 'ai'), on every leg of a
// copied decision at once. The plan then reads as taken from the AI, the
// trader edits it like any other, and a plan they cleared stays cleared:
// a cycle is judged once (the stored suggestion is the record of that),
// and a failed attempt waits autoPlanRetry before the next. Carried
// positions (open for days) are left alone, as everywhere else.

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

// autoPlanUser starts a suggestion-and-apply for each of the user's
// unplanned open decisions and reports how many it started. The work runs
// as the user (their provider, their language), like the sheet's button.
func (h *Handlers) autoPlanUser(ctx context.Context, uid int64) int {
	u, err := h.rq().GetUserByID(ctx, uid)
	if err != nil || u.AutoPlan == 0 || u.IsActive == 0 {
		return 0
	}
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
		if v.AvgEntry == nil || v.Qty == 0 || v.Stale || v.HasPlan() || !leadsDecision(v) {
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
			return h.autoPlanPosition(ctx, accountID, symbol)
		})
		started++
	}
	return started
}

// leadsDecision reports whether the leg is the one to plan its decision
// from: a position on its own, or the earliest leg of a set (the one the
// guardrail keys on too) while no leg has a plan - a set with a plan on
// another leg is left to inheritance.
func leadsDecision(v positionView) bool {
	for _, l := range v.Legs {
		if l.HasPlan() {
			return false
		}
		if l.OpenedAt.Before(v.OpenedAt) || (l.OpenedAt.Equal(v.OpenedAt) && l.AccountID < v.AccountID) {
			return false
		}
	}
	return true
}

// autoPlanPosition runs the suggestion and writes it into the plan, unless
// the position closed or got a plan from the trader meanwhile.
func (h *Handlers) autoPlanPosition(ctx context.Context, accountID int64, symbol string) error {
	if err := h.suggestPositionLevels(ctx, accountID, symbol); err != nil {
		return err
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

// userAutoPlan reports whether the caller turned automatic plans on.
func (h *Handlers) userAutoPlan(ctx context.Context) bool {
	u, err := h.rq().GetUserByID(ctx, userID(ctx))
	return err == nil && u.AutoPlan != 0
}

// derefStr reads an optional string.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
