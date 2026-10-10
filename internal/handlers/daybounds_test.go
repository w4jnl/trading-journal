package handlers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// A fill closed at 22:30Z on Oct 9 is 00:30 on Oct 10 in Europe/Amsterdam.
// Every day-scoped lister must put it on Oct 10 - the date the calendar
// (daily_stats, bucketed in Go) already shows - and not on Oct 9, which is
// what a local-midnight bound compared as UTC text produced.
func TestDayBoundsMatchTradingTimezone(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	conn := dbtest.Open(t, "bootstrap")
	q := models.New(conn)
	ctx := context.Background()
	user, err := q.CreateUser(ctx, models.CreateUserParams{Username: "ams"})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := q.CreateAccount(ctx, models.CreateAccountParams{
		UserID: user.ID, Name: "A", ImportSource: "motivewave_csv", AccountRef: "APEX-A", Currency: "USD", CommissionSource: "rate",
	})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(exit time.Time, net float64) {
		entry := exit.Add(-10 * time.Minute)
		if _, err := q.CreateTrade(ctx, models.CreateTradeParams{
			AccountID: acct.ID, Symbol: "MESZ6", SymbolRoot: "MES", Direction: "long",
			Qty: 1, EntryPrice: 5000, ExitPrice: ptrF(5001), EntryTime: entry, ExitTime: &exit,
			NetPnl: ptrF(net), Status: "closed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(time.Date(2026, 10, 9, 21, 0, 0, 0, time.UTC), 10)   // 23:00 CEST Oct 9
	mk(time.Date(2026, 10, 9, 22, 30, 0, 0, time.UTC), 20)  // 00:30 CEST Oct 10
	mk(time.Date(2026, 10, 10, 13, 30, 0, 0, time.UTC), 30) // 15:30 CEST Oct 10
	mk(time.Date(2026, 10, 10, 22, 30, 0, 0, time.UTC), 40) // 00:30 CEST Oct 11

	h := &Handlers{DB: conn, Queries: q, Timezone: ams}
	uctx := auth.WithUser(ctx, &auth.User{ID: user.ID, Username: "ams"})
	oct10 := time.Date(2026, 10, 10, 0, 0, 0, 0, ams)

	// Dashboard day stats.
	st, err := h.dashboardStatsFor(uctx, oct10, ams)
	if err != nil {
		t.Fatal(err)
	}
	if st.Trades != 2 || st.NetPnl < 49.999 || st.NetPnl > 50.001 {
		t.Errorf("dashboardStatsFor(Oct 10) = %d trades, net %.2f; want the 00:30 and 15:30 CEST trades (2, 50.00)", st.Trades, st.NetPnl)
	}
	// Journal day list.
	day, err := loadDayTrades(uctx, conn, oct10)
	if err != nil {
		t.Fatal(err)
	}
	if len(day) != 2 {
		t.Errorf("loadDayTrades(Oct 10) = %d trades, want 2", len(day))
	}
	// Prepare-day loader.
	prep, err := h.journalSvc().LoadDayTrades(uctx, userID(uctx), oct10)
	if err != nil {
		t.Fatal(err)
	}
	if len(prep) != 2 {
		t.Errorf("loadDayTradesForPrep(Oct 10) = %d trades, want 2", len(prep))
	}
	// Week list: ISO week starting Mon Oct 5 covers all four.
	week, err := loadWeekTrades(uctx, conn, time.Date(2026, 10, 5, 0, 0, 0, 0, ams))
	if err != nil {
		t.Fatal(err)
	}
	if len(week) != 4 {
		t.Errorf("loadWeekTrades = %d, want 4", len(week))
	}
	// Analytics date filter via the page: from=to=2026-10-10.
	req := httptest.NewRequest("GET", "/analytics?from=2026-10-10&to=2026-10-10", nil).WithContext(uctx)
	af, err := h.resolveAccountFilter(req)
	if err != nil {
		t.Fatal(err)
	}
	from, _ := parseLocalDay("2026-10-10", ams)
	fu, tu := from.UTC(), from.AddDate(0, 0, 1).UTC()
	trades, err := h.fetchAnalyticsTrades(uctx, af, &fu, &tu)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 2 {
		t.Errorf("analytics Oct 10 = %d trades, want 2", len(trades))
	}

	// Pure helper: bounds are UTC instants.
	f, to := dayBoundsUTC(oct10)
	if f.Location() != time.UTC || to.Location() != time.UTC || !f.Equal(time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC)) || to.Sub(f) != 24*time.Hour {
		t.Errorf("dayBoundsUTC = %v .. %v", f, to)
	}
}

// Trailing-strip averages divide trade-level sums by trade-level counts.
func TestTrailingStripAverages(t *testing.T) {
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	daily := []dailyPnL{{
		Date: day, Net: 50, Trades: 3, Wins: 2, Losses: 1, SumWins: 200, SumLosses: -150,
	}}
	s := buildTrailingStrip(daily)
	if s.AvgWin < 99.999 || s.AvgWin > 100.001 {
		t.Errorf("AvgWin = %v, want 100 (200 over 2 wins), not the day net over the win count", s.AvgWin)
	}
	if s.AvgLoss < -150.001 || s.AvgLoss > -149.999 {
		t.Errorf("AvgLoss = %v, want -150", s.AvgLoss)
	}
}

// The trades page echoes the trading-timezone day it filtered on, not the
// UTC date of the bound (which is the previous day east of UTC).
func TestTradeFiltersEchoTradingDay(t *testing.T) {
	ams, _ := time.LoadLocation("Europe/Amsterdam")
	day := time.Date(2026, 4, 30, 0, 0, 0, 0, ams)
	fromU, toU := day.UTC(), day.AddDate(0, 0, 1).UTC()
	f := tradeFilters{From: &fromU, To: &toU, FromDay: "2026-04-30", ToDay: "2026-04-30"}
	if f.fromLabel() != "2026-04-30" || f.toLabel() != "2026-04-30" {
		t.Errorf("labels = %s / %s, want 2026-04-30", f.fromLabel(), f.toLabel())
	}
	form := filtersAsForm(f)
	if form["From"] != "2026-04-30" || form["To"] != "2026-04-30" {
		t.Errorf("form echo = %v", form)
	}
	// Without the day labels (a filter built elsewhere) the UTC date shows.
	bare := tradeFilters{From: &fromU, To: &toU}
	if bare.fromLabel() != "2026-04-29" {
		t.Errorf("fallback label = %s", bare.fromLabel())
	}
}
