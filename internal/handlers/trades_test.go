package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// The default trades list honors the Settings account-visibility flag
// (excluded_from_stats) the way Calendar/Analytics do; an explicit account
// filter overrides it; IncludeHidden (the page's shadow count) lifts it. The
// same buildWhere serves /trades, /api/v1/trades and the MCP list_trades.
func TestTradesListHonorsAccountVisibility(t *testing.T) {
	conn, q := newAuthzTestDB(t)
	bg := context.Background()
	u, err := q.CreateUser(bg, models.CreateUserParams{Username: "vis"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithUser(bg, &auth.User{ID: u.ID, Username: "vis", DisplayName: "V"})
	mk := func(name string) models.Account {
		a, err := q.CreateAccount(bg, models.CreateAccountParams{
			UserID: u.ID, Name: name, ImportSource: "motivewave_csv", AccountRef: name,
			Currency: "USD", CommissionSource: "rate",
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return a
	}
	shown, hidden := mk("shown"), mk("hidden")
	if err := q.SetAccountExcluded(bg, models.SetAccountExcludedParams{ExcludedFromStats: 1, ID: hidden.ID, UserID: u.ID}); err != nil {
		t.Fatal(err)
	}
	exit := time.Now().UTC().Add(-time.Hour)
	for _, acc := range []int64{shown.ID, hidden.ID} {
		if _, err := q.CreateTrade(bg, models.CreateTradeParams{
			AccountID: acc, Symbol: "MESU6", SymbolRoot: "MES", Direction: "long", Qty: 1,
			EntryPrice: 5000, ExitPrice: ptrF(5010), EntryTime: exit.Add(-time.Hour), ExitTime: &exit,
			NetPnl: ptrF(50), Status: "closed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	accounts := func(f tradeFilters) []int64 {
		rows, err := listTradesFiltered(ctx, conn, f)
		if err != nil {
			t.Fatal(err)
		}
		n, err := countTradesFiltered(ctx, conn, f)
		if err != nil {
			t.Fatal(err)
		}
		if int(n) != len(rows) {
			t.Errorf("count %d != rows %d", n, len(rows))
		}
		out := make([]int64, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.AccountID)
		}
		return out
	}

	// Default: the hidden account's trade is out.
	if got := accounts(tradeFilters{Page: 1}); len(got) != 1 || got[0] != shown.ID {
		t.Errorf("default list = %v, want only account %d", got, shown.ID)
	}
	// Naming the hidden account in the filter overrides the rule.
	if got := accounts(tradeFilters{Page: 1, Account: accountFilter{Active: true, AccountIDs: []int64{hidden.ID}}}); len(got) != 1 || got[0] != hidden.ID {
		t.Errorf("explicit filter = %v, want only account %d", got, hidden.ID)
	}
	// The shadow count's IncludeHidden lifts it without a filter.
	if got := accounts(tradeFilters{Page: 1, IncludeHidden: true}); len(got) != 2 {
		t.Errorf("IncludeHidden = %v, want both accounts", got)
	}
	// show_retired is a separate concept and does not reveal hidden accounts.
	if got := accounts(tradeFilters{Page: 1, IncludeRetired: true}); len(got) != 1 || got[0] != shown.ID {
		t.Errorf("IncludeRetired = %v, want only account %d", got, shown.ID)
	}
}
