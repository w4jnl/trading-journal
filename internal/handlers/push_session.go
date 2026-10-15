package handlers

import (
	"context"
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
	for _, p := range positions {
		st.OpenCount++
		sp := push.SessionPosition{AccountID: p.AccountID, Symbol: p.Symbol, Direction: p.Direction, Qty: p.Qty, Beyond: p.BeyondStop}
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
	// The largest exposure first, so the three that fit are the ones that
	// matter.
	for i := 1; i < len(st.Positions); i++ {
		for j := i; j > 0 && absf(st.Positions[j].Unrealized) > absf(st.Positions[j-1].Unrealized); j-- {
			st.Positions[j], st.Positions[j-1] = st.Positions[j-1], st.Positions[j]
		}
	}
	if rep := h.coachReport(ctx, today); rep != nil {
		st.Trades = max(st.Trades, rep.Trades)
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

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
