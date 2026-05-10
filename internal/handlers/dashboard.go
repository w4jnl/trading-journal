package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
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

	today := todayLocal(tz)
	isStale := stats.HasTrades && !stats.Date.Equal(today)

	h.renderPage(w, "dashboard.html", map[string]any{
		"Stats":        stats,
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
