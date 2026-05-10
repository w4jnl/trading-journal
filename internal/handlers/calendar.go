package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jaro/trading-journal/internal/models"
)

// dayCell is what the template renders for each day in the grid.
type dayCell struct {
	Date       time.Time
	InMonth    bool // false for spillover from prev/next month
	HasData    bool
	Trades     int64
	Wins       int64
	Losses     int64
	NetPnl     float64
	GrossPnl   float64
	Commission float64
}

// CalendarPage renders /calendar — a monthly grid colored by net P&L.
// Query params: month=YYYY-MM (default current), account_id (default all).
func (h *Handlers) CalendarPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()

	monthStart, err := parseMonth(r.URL.Query().Get("month"), tz)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	monthEnd := monthStart.AddDate(0, 1, 0)

	var accountID *int64
	if v := r.URL.Query().Get("account_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			accountID = &id
		}
	}

	rows, err := h.Queries.ListDailyStatsForRange(ctx, models.ListDailyStatsForRangeParams{
		StatDate:   monthStart,
		StatDate_2: monthEnd,
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("list daily_stats: %w", err))
		return
	}

	// Aggregate per local-day key.
	byDate := map[string]*dayCell{}
	for _, row := range rows {
		if accountID != nil && row.AccountID != *accountID {
			continue
		}
		y, m, d := row.StatDate.In(tz).Date()
		key := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
		c, ok := byDate[key]
		if !ok {
			c = &dayCell{Date: time.Date(y, m, d, 0, 0, 0, 0, tz)}
			byDate[key] = c
		}
		c.HasData = true
		c.Trades += row.TradesCount
		c.Wins += row.Wins
		c.Losses += row.Losses
		c.NetPnl += row.NetPnl
		c.GrossPnl += row.GrossPnl
		c.Commission += row.Commission
	}

	// Sunday-start grid covering 6 weeks (always covers any month).
	gridStart := monthStart
	for gridStart.Weekday() != time.Sunday {
		gridStart = gridStart.AddDate(0, 0, -1)
	}
	weeks := make([][]dayCell, 6)
	for wk := 0; wk < 6; wk++ {
		row := make([]dayCell, 7)
		for d := 0; d < 7; d++ {
			day := gridStart.AddDate(0, 0, wk*7+d)
			key := day.Format("2006-01-02")
			cell := dayCell{Date: day, InMonth: day.Month() == monthStart.Month()}
			if c, ok := byDate[key]; ok {
				cell.HasData = c.HasData
				cell.Trades = c.Trades
				cell.Wins = c.Wins
				cell.Losses = c.Losses
				cell.NetPnl = c.NetPnl
				cell.GrossPnl = c.GrossPnl
				cell.Commission = c.Commission
			}
			row[d] = cell
		}
		weeks[wk] = row
	}

	// Month totals.
	var monthNet, monthGross, monthComm float64
	var monthTrades, winDays, lossDays int64
	for _, week := range weeks {
		for _, c := range week {
			if !c.InMonth || !c.HasData {
				continue
			}
			monthNet += c.NetPnl
			monthGross += c.GrossPnl
			monthComm += c.Commission
			monthTrades += c.Trades
			switch {
			case c.NetPnl > 0:
				winDays++
			case c.NetPnl < 0:
				lossDays++
			}
		}
	}

	accounts, err := h.Queries.ListAccounts(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}

	// Templates can't compare *int64 to int64; pass the dereferenced value
	// (0 = "no filter", which is fine since account IDs start at 1).
	var accountIDForTmpl int64
	if accountID != nil {
		accountIDForTmpl = *accountID
	}

	h.renderPage(w, "calendar.html", map[string]any{
		"MonthLabel":    monthStart.Format("January 2006"),
		"MonthQS":       monthStart.Format("2006-01"),
		"PrevMonth":     monthStart.AddDate(0, -1, 0).Format("2006-01"),
		"NextMonth":     monthStart.AddDate(0, 1, 0).Format("2006-01"),
		"Weeks":         weeks,
		"MonthNet":      monthNet,
		"MonthGross":    monthGross,
		"MonthComm":     monthComm,
		"Trades":        monthTrades,
		"WinDays":       winDays,
		"LossDays":      lossDays,
		"Accounts":      accounts,
		"AccountID":     accountIDForTmpl,
	})
}

func parseMonth(s string, tz *time.Location) (time.Time, error) {
	if s == "" {
		now := time.Now().In(tz)
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, tz), nil
	}
	t, err := time.ParseInLocation("2006-01", s, tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("month must be YYYY-MM")
	}
	return t, nil
}
