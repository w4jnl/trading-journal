package handlers

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// copyFillLine is one fill in a per-(symbol, side) inspection list.
type copyFillLine struct {
	Time  string
	Qty   float64
	Price float64
	Cum   float64 // running volume on this side for this account
	Ref   string
}

// copyLine compares one (symbol, side) between a follower account and factor x
// a leader account, on GROSS traded volume - which stays meaningful even when
// both accounts are flat (net 0). A missing/extra/partial copied fill shows as
// a nonzero drift.
type copyLine struct {
	Symbol   string
	Side     string // "buy" | "sell"
	Leader   float64
	Expected float64 // factor * Leader
	Follower float64
	Drift    float64 // Follower - Expected
	Diverged bool
	// Populated only when diverged: both accounts' fills of this side, so the
	// user can see where the follower fell behind (or ran ahead).
	LeaderFills   []copyFillLine
	FollowerFills []copyFillLine
}

// CopyReconcilePage (GET /reconcile/copy) checks whether a follower account
// mirrored a leader at a given factor over a date range, per symbol and side,
// on gross traded volume. Read-only; scoped to the caller's own accounts.
func (h *Handlers) CopyReconcilePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)
	tz := h.localTZ()

	accounts, err := h.Queries.ListAccounts(ctx, uid)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	own := map[int64]models.Account{}
	for _, a := range accounts {
		own[a.ID] = a
	}

	q := r.URL.Query()
	leaderID, _ := strconv.ParseInt(q.Get("leader"), 10, 64)
	followerID, _ := strconv.ParseInt(q.Get("follower"), 10, 64)
	factor, ferr := strconv.ParseFloat(q.Get("factor"), 64)
	if ferr != nil || factor <= 0 {
		factor = 2
	}
	// Date range in the trading tz; defaults to today (the "did today's copy
	// work?" case). Empty end = through now.
	fromStr := q.Get("from")
	toStr := q.Get("to")
	if q.Get("leader") == "" && q.Get("follower") == "" && fromStr == "" && toStr == "" {
		fromStr = time.Now().In(tz).Format("2006-01-02")
		toStr = fromStr
	}
	from, fromOK := parseDayStart(fromStr, tz)
	toEnd, toOK := parseDayEnd(toStr, tz)

	data := map[string]any{
		"NavActive": "accounts",
		"BackHref":  validBackHref(r),
		"Accounts":  accounts,
		"LeaderID":  leaderID,
		"Follower":  followerID,
		"Factor":    factor,
		"From":      fromStr,
		"To":        toStr,
	}

	leader, lok := own[leaderID]
	follower, fok := own[followerID]
	if !lok || !fok || leaderID == followerID {
		if q.Get("leader") != "" || q.Get("follower") != "" {
			data["Err"] = "Pick two different accounts you own (leader = the 1x source, follower = the copy)."
		}
		h.renderPage(w, r, "copy_reconcile.html", data)
		return
	}

	var fromP, toP *time.Time
	if fromOK {
		fromP = &from
	}
	if toOK {
		toP = &toEnd
	}
	lines, err := h.compareCopyAccounts(ctx, leaderID, followerID, factor, fromP, toP)
	if err != nil {
		h.serverError(w, fmt.Errorf("compare: %w", err))
		return
	}
	diverged := 0
	for _, l := range lines {
		if l.Diverged {
			diverged++
		}
	}
	data["LeaderName"] = leader.Name
	data["FollowerName"] = follower.Name
	data["Lines"] = lines
	data["DivergedCount"] = diverged
	data["Compared"] = true
	h.renderPage(w, r, "copy_reconcile.html", data)
}

// compareCopyAccounts computes the per-(symbol, side) gross-volume comparison
// over [from, to) (nil bounds = unbounded), from both accounts' live fills.
func (h *Handlers) compareCopyAccounts(ctx context.Context, leaderID, followerID int64, factor float64, from, to *time.Time) ([]copyLine, error) {
	tz := h.localTZ()
	leaderEx, err := h.Queries.ListExecutionsByAccount(ctx, leaderID)
	if err != nil {
		return nil, err
	}
	followerEx, err := h.Queries.ListExecutionsByAccount(ctx, followerID)
	if err != nil {
		return nil, err
	}

	type key struct {
		symbol string
		side   string
	}
	type agg struct {
		leaderVol, followerVol float64
		leaderFills            []copyFillLine
		followerFills          []copyFillLine
	}
	m := map[key]*agg{}
	inRange := func(t time.Time) bool {
		if from != nil && t.Before(*from) {
			return false
		}
		if to != nil && !t.Before(*to) {
			return false
		}
		return true
	}
	collect := func(exs []models.Execution, follower bool) {
		for _, e := range exs {
			if e.DeletedAt != nil || !inRange(e.ExecTime) {
				continue
			}
			k := key{e.Symbol, e.Side}
			a := m[k]
			if a == nil {
				a = &agg{}
				m[k] = a
			}
			fl := copyFillLine{
				Time: e.ExecTime.In(tz).Format("2006-01-02 15:04:05"), Qty: e.Qty, Price: e.Price, Ref: e.BrokerExecID,
			}
			if follower {
				a.followerVol += e.Qty
				a.followerFills = append(a.followerFills, fl)
			} else {
				a.leaderVol += e.Qty
				a.leaderFills = append(a.leaderFills, fl)
			}
		}
	}
	collect(leaderEx, false)
	collect(followerEx, true)

	lines := make([]copyLine, 0, len(m))
	for k, a := range m {
		expected := factor * a.leaderVol
		drift := a.followerVol - expected
		line := copyLine{
			Symbol: k.symbol, Side: k.side,
			Leader: a.leaderVol, Expected: expected, Follower: a.followerVol, Drift: drift,
			Diverged: math.Abs(drift) > 1e-9,
		}
		if line.Diverged {
			line.LeaderFills = runningCum(a.leaderFills)
			line.FollowerFills = runningCum(a.followerFills)
		}
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].Diverged != lines[j].Diverged {
			return lines[i].Diverged // problems first
		}
		if lines[i].Symbol != lines[j].Symbol {
			return lines[i].Symbol < lines[j].Symbol
		}
		return lines[i].Side < lines[j].Side
	})
	return lines, nil
}

// runningCum sorts fills by time and fills the Cum column (fills arrive
// account-ordered from ListExecutionsByAccount, already by exec_time).
func runningCum(fills []copyFillLine) []copyFillLine {
	var cum float64
	for i := range fills {
		cum += fills[i].Qty
		fills[i].Cum = cum
	}
	return fills
}

func parseDayStart(s string, tz *time.Location) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", s, tz)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func parseDayEnd(s string, tz *time.Location) (time.Time, bool) {
	t, ok := parseDayStart(s, tz)
	if !ok {
		return time.Time{}, false
	}
	return t.AddDate(0, 0, 1), true // exclusive end = start of next day
}
