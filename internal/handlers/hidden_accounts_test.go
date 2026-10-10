package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// An account hidden in Settings (excluded_from_stats) leaves the DEFAULT
// trades list, calendar and analytics; the journal must not let it back in
// through the side door. prepare_day (MCP / API / the AI reviewer), the
// journal day and week tables and the group editor's link candidates all
// skip its trades; the explicitly linked and the per-trade entry still load.
func TestHiddenAccountsStayOutOfTheJournal(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref, excluded_from_stats) VALUES
			(1, ` + itoa64(uid) + `, 'visible', 'motivewave_csv', 'A-1', 0),
			(2, ` + itoa64(uid) + `, 'hidden', 'motivewave_csv', 'A-2', 1)`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
			(10, 1, 'ESU6', 'ES', 'long', 1, 100, 101, '2026-10-08 14:00:00 +0000 UTC', '2026-10-08 15:00:00 +0000 UTC', 'closed', 50),
			(20, 2, 'ESU6', 'ES', 'long', 1, 100, 101, '2026-10-08 16:00:00 +0000 UTC', '2026-10-08 17:00:00 +0000 UTC', 'closed', 500),
			(21, 2, 'ESU6', 'ES', 'short', 1, 100, 99, '2026-10-09 16:00:00 +0000 UTC', '2026-10-09 17:00:00 +0000 UTC', 'closed', 500)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

	prep, err := h.journalSvc().LoadDayTrades(ctx, uid, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(prep) != 1 || prep[0].ID != 10 {
		t.Errorf("prepare-day loader = %v, want only trade 10", ids(prep))
	}
	// The hidden account traded on the 9th; the last trading day is the 8th.
	last, err := h.lastTradingDay(ctx, time.UTC)
	if err != nil || !last.Equal(day) {
		t.Errorf("lastTradingDay = %v (%v), want %v", last, err, day)
	}
	resp, err := h.prepareDayEntries(ctx, "2026-10-08")
	if err != nil {
		t.Fatal(err)
	}
	if entries := resp.Entries; len(entries) != 1 || entries[0].TradeIDs[0] != 10 {
		t.Errorf("prepare_day entries = %+v, want one for trade 10", entries)
	}

	dayRows, err := loadDayTrades(ctx, conn, day)
	if err != nil || len(dayRows) != 1 || dayRows[0].ID != 10 {
		t.Errorf("journal day table = %v (%v), want only trade 10", linkedIDs(dayRows), err)
	}
	weekRows, err := loadWeekTrades(ctx, conn, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil || len(weekRows) != 1 || weekRows[0].ID != 10 {
		t.Errorf("journal week table = %v (%v), want only trade 10", linkedIDs(weekRows), err)
	}
	cands, err := loadLinkableTrades(ctx, conn, uid, 0, 50)
	if err != nil || len(cands) != 1 || cands[0].ID != 10 {
		t.Errorf("group link candidates = %v (%v), want only trade 10", linkedIDs(cands), err)
	}
	// Explicit per-trade access is untouched: the trade still exists.
	one, err := loadOneTrade(ctx, conn, 20)
	if err != nil || len(one) != 1 {
		t.Errorf("loadOneTrade(hidden) = %v (%v), want the trade", linkedIDs(one), err)
	}
}

func ids(ts []prepDayTrade) []int64 {
	out := make([]int64, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}

func linkedIDs(ts []linkedTrade) []int64 {
	out := make([]int64, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
