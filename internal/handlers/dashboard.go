package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
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

	lastSyncs, err := h.Queries.ListLastSyncByType(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list last syncs: %w", err))
		return
	}

	recent, err := listTradesFiltered(ctx, h.DB, tradeFilters{Page: 1})
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

	today := todayLocal(tz)
	isStale := stats.HasTrades && !stats.Date.Equal(today)

	h.renderPage(w, "dashboard.html", map[string]any{
		"NavActive":    "dashboard",
		"BackHref":     validBackHref(r),
		"Stats":        stats,
		"Equity":       equity,
		"DateLabel":    stats.Date.Format("Mon, 2 Jan 2006"),
		"IsStale":      isStale,
		"LastSyncs":    lastSyncs,
		"RecentTrades": recent,
	})
}

// pickDashboardDate returns today (in trading TZ) if there are closed trades
// for it, else the latest day with closed trades, else today (empty state).
func (h *Handlers) pickDashboardDate(ctx context.Context, tz *time.Location) (time.Time, error) {
	today := todayLocal(tz)
	tomorrow := today.AddDate(0, 0, 1)

	var hasToday int
	err := h.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trades WHERE status='closed' AND exit_time >= ? AND exit_time < ?`,
		today, tomorrow,
	).Scan(&hasToday)
	if err != nil {
		return time.Time{}, err
	}
	if hasToday > 0 {
		return today, nil
	}

	// MAX(exit_time) loses the column type and the driver can't scan an
	// aggregate string back into time.Time. ORDER BY ... LIMIT 1 keeps the
	// column-typed scan path.
	var mostRecent time.Time
	err = h.DB.QueryRowContext(ctx,
		`SELECT exit_time FROM trades WHERE status='closed' AND exit_time IS NOT NULL ORDER BY exit_time DESC LIMIT 1`,
	).Scan(&mostRecent)
	if errors.Is(err, sql.ErrNoRows) {
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
	end := date.AddDate(0, 0, 1)
	rows, err := h.DB.QueryContext(ctx, `
		SELECT direction, qty, gross_pnl, commission, net_pnl
		FROM trades
		WHERE status='closed' AND exit_time IS NOT NULL
		  AND exit_time >= ? AND exit_time < ?
	`, date, end)
	if err != nil {
		return dashboardStats{}, err
	}
	defer rows.Close()

	stats := dashboardStats{Date: date}
	first := true
	for rows.Next() {
		var direction string
		var qty float64
		var gross, comm, net *float64
		if err := rows.Scan(&direction, &qty, &gross, &comm, &net); err != nil {
			return dashboardStats{}, err
		}
		stats.Trades++
		stats.HasTrades = true
		if direction == "long" {
			stats.Longs++
		} else {
			stats.Shorts++
		}
		if gross != nil {
			stats.GrossPnl += *gross
		}
		if comm != nil {
			stats.Commission += *comm
		}
		if net != nil {
			stats.NetPnl += *net
			switch {
			case *net > 0:
				stats.Wins++
			case *net < 0:
				stats.Losses++
			}
			if first || *net > stats.BestNetPnl {
				stats.BestNetPnl = *net
			}
			if first || *net < stats.WorstNetPnl {
				stats.WorstNetPnl = *net
			}
			first = false
		}
	}
	if stats.Wins+stats.Losses > 0 {
		stats.WinRate = float64(stats.Wins) / float64(stats.Wins+stats.Losses)
	}
	return stats, rows.Err()
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
	accounts, err := h.Queries.ListAccounts(ctx)
	if err != nil {
		return dashboardEquity{}, err
	}
	var eq dashboardEquity
	eq.AsOf = todayLocal(tz).Format("2006-01-02")
	byType := map[string]*equityByType{}
	for _, a := range accounts {
		if a.Active != 1 {
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
