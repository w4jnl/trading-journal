package guardrail

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// ---- Open positions ----------------------------------------------------
//
// The rules above read closed trades; these read what is open right now
// (positions.OpenLegs), on every tick, since "still no stop" is a matter of
// the clock, not of new data. A copied decision is one thing to say: the
// legs of a set share the nudge, keyed on the earliest leg.

const (
	// unplannedAfter is how long a position may be open before the missing
	// stop is pointed out: a few minutes to type it in after the fill.
	unplannedAfter = 3 * time.Minute
)

// openSet is one decision's open legs.
type openSet struct {
	legs []positions.OpenLeg
}

func (s openSet) first() positions.OpenLeg {
	f := s.legs[0]
	for _, l := range s.legs[1:] {
		if l.Position.Opened.ExecTime.Before(f.Position.Opened.ExecTime) {
			f = l
		}
	}
	return f
}

// openSets groups the legs into decisions (sets, plus every leg on its own).
func openSets(legs []positions.OpenLeg) []openSet {
	inSet := map[int]bool{}
	var out []openSet
	for _, g := range positions.ClusterOpen(legs) {
		s := openSet{}
		for _, k := range g {
			s.legs = append(s.legs, legs[k])
			inSet[k] = true
		}
		out = append(out, s)
	}
	for i, l := range legs {
		if !inSet[i] {
			out = append(out, openSet{legs: []positions.OpenLeg{l}})
		}
	}
	return out
}

// positionHref is the position's sheet, deep-linked on /trades (the same
// link the handlers build).
func positionHref(accountID int64, symbol string) string {
	q := url.Values{"status": {"open"}, "sheet": {"pos:" + strconv.FormatInt(accountID, 10) + ":" + symbol}}
	return "/trades?" + q.Encode()
}

// fromOpenPositions says when a position has been open for a few minutes
// with no stop recorded, and when the planned risk of what is open on an
// account exceeds what the day plan's max loss still allows after the
// day's realised result. Both only while the opening fill is fresh: a
// position carried from a previous session is not a moment to nudge.
func fromOpenPositions(legs []positions.OpenLeg, names map[int64]string, plan *dayplan.Plan, trades []dayTrade, now time.Time, fresh time.Duration) []Nudge {
	var out []Nudge
	for _, s := range openSets(legs) {
		f := s.first()
		opened := f.Position.Opened.ExecTime
		if !isFresh(now, opened, fresh) || now.Sub(opened) < unplannedAfter {
			continue
		}
		unplanned := true
		for _, l := range s.legs {
			if l.Plan != nil && (l.Plan.Stop != nil || l.Plan.Risk != nil) {
				unplanned = false
			}
		}
		if !unplanned {
			continue
		}
		what := fmt.Sprintf("%s %g %s", f.Direction(), f.Qty(), f.Symbol)
		if len(s.legs) > 1 {
			what = fmt.Sprintf("%s %s on %d accounts", f.Direction(), f.Symbol, len(s.legs))
		}
		out = append(out, Nudge{Key: "unplanned-open", Ref: strconv.FormatInt(f.Position.Opened.ID, 10), Severity: "warn", Mistake: "no-stop",
			Title:  "No stop recorded on " + f.Symbol,
			Detail: fmt.Sprintf("Open %s since %s with no plan: record the stop, or the journal cannot say what is at risk.", what, opened.In(now.Location()).Format("15:04")),
			Href:   positionHref(f.AccountID, f.Symbol)})
	}

	if plan == nil || plan.MaxLoss == nil || *plan.MaxLoss <= 0 {
		return out
	}
	limit := *plan.MaxLoss
	realised := map[int64]float64{}
	for _, t := range trades {
		realised[t.AccountID] += t.Net
	}
	type acct struct {
		risk   float64
		newest time.Time
		symbol string
	}
	byAcct := map[int64]*acct{}
	var order []int64
	for _, l := range legs {
		if l.Plan == nil || l.Plan.Risk == nil {
			continue
		}
		a := byAcct[l.AccountID]
		if a == nil {
			a = &acct{}
			byAcct[l.AccountID] = a
			order = append(order, l.AccountID)
		}
		a.risk += *l.Plan.Risk
		if t := l.Position.Opened.ExecTime; t.After(a.newest) {
			a.newest, a.symbol = t, l.Symbol
		}
	}
	for _, id := range order {
		a := byAcct[id]
		if !isFresh(now, a.newest, fresh) {
			continue
		}
		left := limit + realised[id] // realised is negative on a losing day
		if left < 0 {
			left = 0
		}
		if a.risk <= left {
			continue
		}
		out = append(out, Nudge{Key: "open-risk", Ref: strconv.FormatInt(id, 10), Severity: "warn", Plan: true, Mistake: "against-plan",
			Title:  "Open risk past the plan on " + names[id],
			Detail: fmt.Sprintf("The stops on what is open risk %.2f; the plan's max loss leaves %.2f today (%.2f realised so far).", a.risk, left, realised[id]),
			Href:   positionHref(id, a.symbol)})
	}
	return out
}

// openLegs reads the user's open positions with their plans, and the
// account names for the nudges.
func (s *Service) openLegs(ctx context.Context, uid int64) ([]positions.OpenLeg, map[int64]string, error) {
	q := models.New(s.rdb())
	legs, err := positions.OpenLegs(ctx, q, uid)
	if err != nil {
		return nil, nil, fmt.Errorf("guardrail: open legs: %w", err)
	}
	names := map[int64]string{}
	for _, l := range legs {
		if _, ok := names[l.AccountID]; ok {
			continue
		}
		if a, err := q.GetAccountByID(ctx, models.GetAccountByIDParams{ID: l.AccountID, UserID: uid}); err == nil {
			names[l.AccountID] = a.Name
		}
	}
	return legs, names, nil
}

// throughStop says, once per leg, when the newest cached price is through
// the recorded stop: below it for a long, above it for a short. The price
// must be recent (within fresh) so a stale cache never cries wolf; the
// wording allows for a stop order that filled and has not arrived yet.
func (s *Service) throughStop(ctx context.Context, legs []positions.OpenLeg, now time.Time) []Nudge {
	if s.LastClose == nil {
		return nil
	}
	var out []Nudge
	prices := map[string]struct {
		p  float64
		at time.Time
		ok bool
	}{}
	for _, l := range legs {
		if l.Plan == nil || l.Plan.Stop == nil {
			continue
		}
		q, seen := prices[l.SymbolRoot]
		if !seen {
			q.p, q.at, q.ok = s.LastClose(ctx, l.SymbolRoot)
			prices[l.SymbolRoot] = q
		}
		if !q.ok || !isFresh(now, q.at, s.fresh()) {
			continue
		}
		stop := *l.Plan.Stop
		through := (l.Direction() == "long" && q.p < stop) || (l.Direction() == "short" && q.p > stop)
		if !through {
			continue
		}
		out = append(out, Nudge{Key: "past-stop-live", Ref: strconv.FormatInt(l.Position.Opened.ID, 10), Severity: "warn", Mistake: "moved-stop",
			Title:  "Price is through your stop on " + l.Symbol,
			Detail: fmt.Sprintf("Last %.2f against the recorded stop %g on %s %g. If the stop order filled, its fill arrives with the next sync; if not, this is the moment the plan was for.", q.p, stop, l.Direction(), l.Qty()),
			Href:   positionHref(l.AccountID, l.Symbol)})
	}
	return out
}
