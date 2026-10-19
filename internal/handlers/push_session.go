package handlers

import (
	"context"
	"sort"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/push"
)

// SessionState is the Session Live Activity's content for a user, built
// from the same loaders as /today: the day's stats, the priced open
// positions (the largest exposure first), the plan's two budgets and the
// unseen nudges. Runs as the user (push.Service.State).
func (h *Handlers) SessionState(ctx context.Context, uid int64) (*push.SessionState, error) {
	u, err := h.rq().GetUserByID(ctx, uid)
	if err != nil {
		return nil, err
	}
	ctx = auth.WithUser(ctx, &auth.User{ID: u.ID, Username: u.Username, DisplayName: derefStr(u.DisplayName), Email: derefStr(u.Email), IsAdmin: u.IsAdmin != 0})
	tz := h.localTZ()
	today := todayLocal(tz)
	dayKey := today.Format("2006-01-02")
	now := time.Now()

	stats, err := h.dailyStats(ctx, accountFilter{}, apiDateRange{From: today, To: today})
	if err != nil {
		return nil, err
	}
	positions, err := h.openPositionsAPI(ctx)
	if err != nil {
		return nil, err
	}
	st := &push.SessionState{Realized: stats.Totals.Net, Trades: int(stats.Totals.Trades), Wins: int(stats.Totals.Wins), Losses: int(stats.Totals.Losses), Positions: []push.SessionPosition{}}
	// Per decision the same as per trade until the coach has counted.
	st.Decisions, st.SetWins, st.SetLosses = st.Trades, st.Wins, st.Losses
	sets := map[string]bool{}
	for _, p := range positions {
		st.OpenCount++
		sp := push.SessionPosition{AccountID: p.AccountID, Symbol: p.Symbol, Direction: p.Direction, Qty: p.Qty, Beyond: p.BeyondStop, Set: sessionSet(p)}
		sets[sp.SetKey()] = true
		if p.AvgEntry != nil {
			sp.AvgEntry = *p.AvgEntry
		}
		if p.LastPrice != nil {
			sp.Last = *p.LastPrice
		}
		if p.Unrealized != nil {
			sp.Unrealized = *p.Unrealized
			st.Unrealized += *p.Unrealized
		}
		if p.LastAt == nil || now.Sub(*p.LastAt) > 5*time.Minute {
			st.Stale = true
		}
		if p.TicksToStop != nil {
			sp.TicksToStop = *p.TicksToStop
		}
		st.Positions = append(st.Positions, sp)
		for _, f := range p.CycleFills {
			if f.Time.Unix() > st.LastFill {
				st.LastFill = f.Time.Unix()
			}
		}
	}
	st.OpenSets = len(sets)
	st.Arrange()
	if rep := h.coachReport(ctx, today); rep != nil {
		st.Trades = max(st.Trades, rep.Trades)
		st.Decisions, st.SetWins, st.SetLosses = rep.Decisions, rep.DecisionWins, rep.DecisionLosses
		if rep.Plan != nil {
			if rep.Plan.Plan.MaxTrades != nil {
				st.MaxTrades = int(*rep.Plan.Plan.MaxTrades)
			}
			if rep.Plan.Plan.MaxLoss != nil {
				st.MaxLoss = *rep.Plan.Plan.MaxLoss
			}
		}
	}
	if p := h.dayPlan(ctx, dayKey); p != nil {
		if p.MaxTrades != nil {
			st.MaxTrades = int(*p.MaxTrades)
		}
		if p.MaxLoss != nil {
			st.MaxLoss = *p.MaxLoss
		}
	}
	var worst float64
	for _, net := range h.dayNetByAccountCtx(ctx, today) {
		if net < worst {
			worst = net
		}
	}
	st.LossUsed = -worst
	if from, to := today.UTC(), today.AddDate(0, 0, 1).UTC(); st.LastFill == 0 {
		if trades, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, &from, &to); err == nil && len(trades) > 0 {
			st.LastFill = trades[len(trades)-1].ExitTime.Unix()
		}
	}
	st.Nudges = int(h.unseenBadgeCtx(ctx, dayKey))
	return st, nil
}

// sessionSet is the copied decision a position belongs to, as the sorted
// account ids of every leg including this one; nil for a position on its
// own (the API lists only the other legs).
func sessionSet(p apiPosition) []int64 {
	if len(p.SetLegs) == 0 {
		return nil
	}
	set := append([]int64{p.AccountID}, p.SetLegs...)
	sort.Slice(set, func(i, j int) bool { return set[i] < set[j] })
	return set
}
