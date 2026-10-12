package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// dashboardStats summarizes the trades for a single trading day.
type dashboardStats struct {
	Date        time.Time
	Trades      int64
	Wins        int64
	Losses      int64
	GrossPnl    float64
	NetPnl      float64
	Commission  float64
	WinRate     float64 // 0..1
	BestNetPnl  float64
	WorstNetPnl float64
	Longs       int64
	Shorts      int64
	HasTrades   bool
}

// dashboardTrailing holds the 30-day trailing stats shown in the dashboard's
// stat strip. Each Spark* field is a 30-element series scaled to the tile's
// own min/max so weak signals still register visually.
type dashboardTrailing struct {
	HasData bool

	NetPnl   float64
	NetSpark []float64 // daily cumulative net P&L, 30 entries

	Trades      int64
	TradesSpark []float64 // daily trade count, 30 entries

	WinRate      float64   // 0..1
	WinRateSpark []float64 // 7-day rolling win rate, 30 entries

	AvgWin  float64
	AvgLoss float64 // signed (negative)

	Streak    int    // consecutive same-sign trading days from latest backward
	StreakDir string // "win" | "loss" | "flat"
}

// dailyPnL is one trading day inside the 60-day window. Used both for the
// hero equity-curve series and for computing the trailing stat strip.
type dailyPnL struct {
	Date   time.Time
	Net    float64
	Trades int64
	Wins   int64
	Losses int64
	// SumWins / SumLosses are the day's per-trade positive and negative
	// nets, so trailing averages divide like with like (trade sums by trade
	// counts). The day's Net alone cannot give an average win: a day with
	// +100, +100, -150 nets +50 but its average win is 100.
	SumWins   float64
	SumLosses float64
}

// dashboardEquity is the at-a-glance equity tile shown atop the dashboard.
// Known is true only when at least one active account has a balance anchor;
// otherwise the tile renders the empty-state link to /accounts.
//
// ByType breaks the total down per account type (live / prop_live / prop_eval
// / prop_funded / paper) so eval money isn't silently mixed in with real
// capital. Only types with at least one anchored account appear.
type dashboardEquity struct {
	Known         bool
	Total         float64
	AsOf          string // YYYY-MM-DD in the trading timezone
	AccountsKnown int64
	AccountsTotal int64 // active accounts considered
	ByType        []equityByType
}

// equityByType is one row of the equity breakdown — the sum across accounts
// of one type. Order is set by the slice it lives in (handler emits a fixed
// display order so real-money types come first).
type equityByType struct {
	Type     string // "live", "prop_live", etc.
	Label    string // "Live", "Prop · Live"
	Total    float64
	Accounts int64
}

// equityTypeOrder is the display order for the per-type breakdown — real
// capital first, eval money last. Mirrors the visual hierarchy a trader
// uses when sizing risk.
var equityTypeOrder = []struct {
	Type  string
	Label string
}{
	{"live", "Live"},
	{"prop_live", "Prop · Live"},
	{"prop_funded", "Prop · Funded"},
	{"prop_eval", "Prop · Eval"},
	{"paper", "Paper"},
}

// DashboardPage renders / — the at-a-glance summary. Shows the most recent
// trading day with activity (preferring today, then walking back), plus a
// last-sync panel and the 5 most recent trades.
func (h *Handlers) DashboardPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	isFragment := liveFragment(r)
	h.TopstepxSync.MaybeEnqueueUser(userID(ctx)) // throttled background API sync; nil-safe
	h.IbkrSync.MaybeEnqueueUser(userID(ctx))     // throttled IBKR gateway poll; nil-safe
	tz := h.localTZ()

	target, err := h.pickDashboardDate(ctx, tz)
	if err != nil {
		h.serverError(w, fmt.Errorf("pick date: %w", err))
		return
	}

	stats, err := h.dashboardStatsFor(ctx, target, tz)
	if err != nil {
		h.serverError(w, fmt.Errorf("dashboard stats: %w", err))
		return
	}

	lastSyncs, err := h.rq().ListLastSyncByType(ctx, userIDPtr(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list last syncs: %w", err))
		return
	}

	recent, err := listTradesFiltered(ctx, h.rdb(), tradeFilters{Page: 1})
	if err != nil {
		h.serverError(w, fmt.Errorf("list recent: %w", err))
		return
	}
	if len(recent) > 5 {
		recent = recent[:5]
	}

	equity, err := h.dashboardEquityNow(ctx, tz)
	if err != nil {
		h.serverError(w, fmt.Errorf("equity: %w", err))
		return
	}

	daily, err := h.dashboardDaily(ctx, tz, 60)
	if err != nil {
		h.serverError(w, fmt.Errorf("dashboard daily: %w", err))
		return
	}
	curveXs, curveYs, curveTotal := buildEquityCurve(daily)
	trailing := buildTrailingStrip(daily)

	today := todayLocal(tz)
	isStale := stats.HasTrades && !stats.Date.Equal(today)

	// Open-position summary: nonzero (account, contract) nets = open/carried/data-gap
	// positions. Empty list => the user is flat. Non-fatal; a lookup error just hides it.
	openPositions, err := h.rq().ListOpenPositions(ctx, userID(ctx))
	if err != nil {
		h.logger().Warn("dashboard open positions", "err", err)
	}

	data := map[string]any{
		"NavActive":     "dashboard",
		"OpenPositions": h.openPositionRows(ctx, openPositions),
		"BackHref":      validBackHref(r),
		"Stats":         stats,
		"Equity":        equity,
		"DateLabel":     stats.Date.Format("Mon, 2 Jan 2006"),
		"TodayDate":     todayLocal(tz).Format("2006-01-02"),
		"TodayPlan":     planSummary(h.dayPlan(ctx, todayLocal(tz).Format("2006-01-02"))),
		"IsStale":       isStale,
		"LastSyncs":     lastSyncs,
		"RecentTrades":  recent,
		"Trailing":      trailing,
		"CurveXs":       curveXs,
		"CurveYs":       curveYs,
		"CurveTotal":    curveTotal,
		"CurveHasData":  len(curveXs) > 0,
	}
	if isFragment {
		h.renderPageFragment(w, r, "dashboard.html", "dash_fragment", data)
		return
	}
	h.renderPage(w, r, "dashboard.html", data)
}

// dashboardDaily returns one row per trading day in the last `days` days,
// ordered by date ASC. Buckets are grouped in Go on the trading TZ so
// midnight boundaries line up with the user's day — SQLite's date() runs
// in UTC and would split a late-session NY trade onto the next day.
// (It also dodges a driver quirk where date() over Go's time.Time storage
// occasionally returned NULL.) Days with no closed trades are omitted.
func (h *Handlers) dashboardDaily(ctx context.Context, tz *time.Location, days int) ([]dailyPnL, error) {
	// Bounds go to SQL as UTC instants (see daybounds.go); bucketing below
	// stays in the trading timezone.
	start, end := daysBoundsUTC(todayLocal(tz).AddDate(0, 0, 1-days), days)
	rows, err := h.rq().DashboardDailyNetPnL(ctx, models.DashboardDailyNetPnLParams{
		ExitTime:   &start,
		ExitTime_2: &end,
		UserID:     userID(ctx),
	})
	if err != nil {
		return nil, err
	}

	bucket := map[time.Time]*dailyPnL{}
	for _, r := range rows {
		if r.ExitTime == nil || r.NetPnl == nil {
			continue
		}
		local := r.ExitTime.In(tz)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, tz)
		row, ok := bucket[day]
		if !ok {
			row = &dailyPnL{Date: day}
			bucket[day] = row
		}
		row.Net += *r.NetPnl
		row.Trades++
		switch {
		case *r.NetPnl > 0:
			row.Wins++
			row.SumWins += *r.NetPnl
		case *r.NetPnl < 0:
			row.Losses++
			row.SumLosses += *r.NetPnl
		}
	}
	out := make([]dailyPnL, 0, len(bucket))
	for _, row := range bucket {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}

// buildEquityCurve turns the dense per-day P&L list into the cumulative-net
// series used by the hero chart. Returns parallel xs (unix-seconds) and ys
// (cumulative floats), prepended with a zero baseline so the curve starts
// at zero. total is the final cumulative value (drives stroke color in JS).
func buildEquityCurve(daily []dailyPnL) (xs []int64, ys []float64, total float64) {
	if len(daily) == 0 {
		return nil, nil, 0
	}
	xs = make([]int64, 0, len(daily)+1)
	ys = make([]float64, 0, len(daily)+1)
	xs = append(xs, daily[0].Date.Add(-12*time.Hour).Unix())
	ys = append(ys, 0)
	cum := 0.0
	for _, d := range daily {
		cum += d.Net
		xs = append(xs, d.Date.Add(12*time.Hour).Unix())
		ys = append(ys, cum)
	}
	return xs, ys, cum
}

// buildTrailingStrip extracts the 30-day-trailing tiles + sparkline series
// from the dense per-day list. Sparklines are min-max normalized later by
// the template helper; this function just packs the raw daily values.
func buildTrailingStrip(daily []dailyPnL) dashboardTrailing {
	t := dashboardTrailing{}
	if len(daily) == 0 {
		return t
	}
	// Anchor on the latest day in the series; walk back up to 30 trading
	// days. Calendar days without trades are intentionally skipped — that's
	// how traders naturally count.
	window := 30
	if len(daily) < window {
		window = len(daily)
	}
	tail := daily[len(daily)-window:]
	t.HasData = true

	cum := 0.0
	cumSeries := make([]float64, 0, len(tail))
	tradesSeries := make([]float64, 0, len(tail))
	winRateSeries := make([]float64, 0, len(tail))
	var wins, losses int64
	var grossWin, grossLoss float64
	for _, d := range tail {
		cum += d.Net
		cumSeries = append(cumSeries, cum)
		tradesSeries = append(tradesSeries, float64(d.Trades))
		wins += d.Wins
		losses += d.Losses
		// Trade-level sums over trade-level counts. Summing positive DAY nets
		// here (as this once did) mixed units: a day with +100, +100, -150
		// contributed +50 to "gross win" and 2 to "wins", showing an average
		// win of 25 against a true 100.
		grossWin += d.SumWins
		grossLoss += d.SumLosses
		t.Trades += d.Trades

		// 7-day rolling win rate, anchored at the end of each daily slice.
		// Without this the sparkline of "win rate" is too jittery to read.
		var rw, rl int64
		look := len(tradesSeries) // index into tail
		from := look - 7
		if from < 0 {
			from = 0
		}
		for i := from; i < look; i++ {
			rw += tail[i].Wins
			rl += tail[i].Losses
		}
		rate := 0.0
		if rw+rl > 0 {
			rate = float64(rw) / float64(rw+rl)
		}
		winRateSeries = append(winRateSeries, rate)
	}
	t.NetPnl = cum
	t.NetSpark = cumSeries
	t.TradesSpark = tradesSeries
	t.WinRateSpark = winRateSeries
	if wins+losses > 0 {
		t.WinRate = float64(wins) / float64(wins+losses)
	}
	if wins > 0 {
		t.AvgWin = grossWin / float64(wins)
	}
	if losses > 0 {
		t.AvgLoss = grossLoss / float64(losses)
	}

	// Streak: walk backward from the latest day, count consecutive same-sign
	// days. Zero-P&L days break the streak.
	last := tail[len(tail)-1].Net
	switch {
	case last > 0:
		t.StreakDir = "win"
	case last < 0:
		t.StreakDir = "loss"
	default:
		t.StreakDir = "flat"
	}
	for i := len(tail) - 1; i >= 0; i-- {
		v := tail[i].Net
		if (t.StreakDir == "win" && v > 0) || (t.StreakDir == "loss" && v < 0) {
			t.Streak++
			continue
		}
		break
	}
	return t
}

// pickDashboardDate returns today (in trading TZ) if there are closed trades
// for it, else the latest day with closed trades, else today (empty state).
func (h *Handlers) pickDashboardDate(ctx context.Context, tz *time.Location) (time.Time, error) {
	today := todayLocal(tz)
	fromU, toU := dayBoundsUTC(today)

	n, err := h.rq().CountClosedTradesInRange(ctx, models.CountClosedTradesInRangeParams{
		ExitTime:   &fromU,
		ExitTime_2: &toU,
		UserID:     userID(ctx),
	})
	if err != nil {
		return time.Time{}, err
	}
	if n > 0 {
		return today, nil
	}

	mostRecent, err := h.rq().MostRecentClosedExitTime(ctx, userID(ctx))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && mostRecent == nil) {
		return today, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	y, m, d := mostRecent.In(tz).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, tz), nil
}

// dashboardStatsFor pulls the closed trades that exited within `[date, date+1d)`
// and aggregates them into the summary the dashboard renders.
func (h *Handlers) dashboardStatsFor(ctx context.Context, date time.Time, tz *time.Location) (dashboardStats, error) {
	fromU, toU := dayBoundsUTC(date)
	rows, err := h.rq().ListClosedTradesInRange(ctx, models.ListClosedTradesInRangeParams{
		ExitTime:   &fromU,
		ExitTime_2: &toU,
		UserID:     userID(ctx),
	})
	if err != nil {
		return dashboardStats{}, err
	}

	stats := dashboardStats{Date: date}
	first := true
	for _, r := range rows {
		stats.Trades++
		stats.HasTrades = true
		if r.Direction == "long" {
			stats.Longs++
		} else {
			stats.Shorts++
		}
		if r.GrossPnl != nil {
			stats.GrossPnl += *r.GrossPnl
		}
		if r.Commission != nil {
			stats.Commission += *r.Commission
		}
		if r.NetPnl != nil {
			stats.NetPnl += *r.NetPnl
			switch {
			case *r.NetPnl > 0:
				stats.Wins++
			case *r.NetPnl < 0:
				stats.Losses++
			}
			if first || *r.NetPnl > stats.BestNetPnl {
				stats.BestNetPnl = *r.NetPnl
			}
			if first || *r.NetPnl < stats.WorstNetPnl {
				stats.WorstNetPnl = *r.NetPnl
			}
			first = false
		}
	}
	if stats.Wins+stats.Losses > 0 {
		stats.WinRate = float64(stats.Wins) / float64(stats.Wins+stats.Losses)
	}
	return stats, nil
}

func todayLocal(tz *time.Location) time.Time {
	now := time.Now().In(tz)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
}

// dashboardEquityNow sums current running balances across active accounts.
// Active = accounts.active=1. Accounts without a balance anchor contribute
// 0 to the total and aren't counted toward AccountsKnown; the tile copy
// surfaces the gap so the user knows what's excluded.
//
// The per-type breakdown follows equityTypeOrder so real capital appears
// above eval / paper money in the rendered table.
func (h *Handlers) dashboardEquityNow(ctx context.Context, tz *time.Location) (dashboardEquity, error) {
	balances, err := h.loadBalances(ctx, time.Now())
	if err != nil {
		return dashboardEquity{}, err
	}
	accounts, err := h.rq().ListAccounts(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		return dashboardEquity{}, err
	}
	var eq dashboardEquity
	eq.AsOf = todayLocal(tz).Format("2006-01-02")
	byType := map[string]*equityByType{}
	for _, a := range accounts {
		if a.Active != 1 || a.ExcludedFromStats != 0 {
			continue
		}
		eq.AccountsTotal++
		b, ok := balances[a.ID]
		if !ok || !b.Known {
			continue
		}
		eq.AccountsKnown++
		eq.Total += b.Balance
		eq.Known = true

		typeSlug := ""
		if a.Type != nil {
			typeSlug = *a.Type
		}
		row, exists := byType[typeSlug]
		if !exists {
			row = &equityByType{Type: typeSlug, Label: equityTypeLabel(typeSlug)}
			byType[typeSlug] = row
		}
		row.Total += b.Balance
		row.Accounts++
	}
	eq.ByType = orderedEquityBreakdown(byType)
	return eq, nil
}

// equityTypeLabel maps an account_type slug to the human label used in the
// breakdown. Falls back to "Other" for unmapped or missing types so a row
// is still rendered (better than hiding anchored money).
func equityTypeLabel(slug string) string {
	for _, ord := range equityTypeOrder {
		if ord.Type == slug {
			return ord.Label
		}
	}
	if slug == "" {
		return "Unspecified"
	}
	return slug
}

// orderedEquityBreakdown returns the per-type rows in equityTypeOrder, then
// any unknown / unspecified types alphabetically at the end so the breakdown
// table always lists the same primary types in the same order.
func orderedEquityBreakdown(byType map[string]*equityByType) []equityByType {
	var out []equityByType
	seen := map[string]bool{}
	for _, ord := range equityTypeOrder {
		if r, ok := byType[ord.Type]; ok {
			out = append(out, *r)
			seen[ord.Type] = true
		}
	}
	var leftovers []equityByType
	for t, r := range byType {
		if seen[t] {
			continue
		}
		leftovers = append(leftovers, *r)
	}
	sort.Slice(leftovers, func(i, j int) bool { return leftovers[i].Label < leftovers[j].Label })
	return append(out, leftovers...)
}
