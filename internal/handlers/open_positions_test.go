package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Open-position rows link to the position's sheet (deep-linked on /trades)
// for every viewer; only admins also get the reconcile editor link, which is
// behind RequireAdmin. Data fields stay promoted for the partial.
func TestOpenPositionRowsLinkPerViewer(t *testing.T) {
	rows := []models.ListOpenPositionsRow{{AccountID: 16, AccountName: "U10000001", Symbol: "ESU6", SymbolRoot: "ES", NetQty: 1}}
	wantHref := "/trades?sheet=pos%3A16%3AESU6&status=open"

	admin := auth.WithUser(context.Background(), &auth.User{ID: 1, Username: "a", IsAdmin: true})
	got := (&Handlers{}).openPositionRows(admin, rows)
	if len(got) != 1 || got[0].Href != wantHref || got[0].AccountName != "U10000001" || got[0].Key != "pos:16:ESU6" || got[0].SheetURL != "/positions/16/ESU6" {
		t.Errorf("admin row = %+v", got)
	}
	if got[0].ReconcileHref != "/admin/transactions?account_id=16&symbol=ESU6" {
		t.Errorf("admin reconcile link = %q", got[0].ReconcileHref)
	}

	user := auth.WithUser(context.Background(), &auth.User{ID: 2, Username: "u"})
	got = (&Handlers{}).openPositionRows(user, rows)
	if len(got) != 1 || got[0].Href != wantHref || got[0].Direction != "long" || got[0].Qty != 1 {
		t.Errorf("non-admin row = %+v", got)
	}
	if got[0].ReconcileHref != "" {
		t.Errorf("non-admin must never be sent to an admin-only page: %q", got[0].ReconcileHref)
	}
	// No user in context (defensive): no admin link.
	if got := (&Handlers{}).openPositionRows(context.Background(), rows); len(got) != 1 || got[0].ReconcileHref != "" {
		t.Errorf("anonymous row = %+v", got)
	}
	// Empty input stays an empty, non-nil slice so the partial renders "flat".
	if got := (&Handlers{}).openPositionRows(user, nil); got == nil || len(got) != 0 {
		t.Errorf("empty = %#v", got)
	}
	// A short position reads as such.
	short := (&Handlers{}).openPositionRows(user, []models.ListOpenPositionsRow{{AccountID: 3, Symbol: "NQU6", SymbolRoot: "NQ", NetQty: -2}})
	if short[0].Direction != "short" || short[0].Qty != 2 {
		t.Errorf("short row = %+v", short[0])
	}
}

// With a database, each open-position row carries the FIFO average entry of
// the contracts still open: after buying 2 @ 6500 and 1 @ 6530 and selling 2,
// the one contract left is the 6530 lot - not the 6510 average of all buys.
func TestOpenPositionRowsAvgEntry(t *testing.T) {
	conn, q := newAuthzTestDB(t)
	ctx := context.Background()
	u, err := q.CreateUser(ctx, models.CreateUserParams{Username: "trader"})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := q.CreateAccount(ctx, models.CreateAccountParams{
		UserID: u.ID, Name: "APEX-1", ImportSource: "motivewave_csv",
		AccountRef: "APEX-1", Currency: "USD", CommissionSource: "rate",
	})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 7, 14, 30, 0, 0, time.UTC)
	for i, f := range []struct {
		side       string
		qty, price float64
	}{{"buy", 2, 6500}, {"buy", 1, 6530}, {"sell", 2, 6510}} {
		if _, err := q.InsertManualExecution(ctx, models.InsertManualExecutionParams{
			AccountID: acct.ID, BrokerExecID: "T-" + string(rune('a'+i)), Symbol: "ESZ6", SymbolRoot: "ES",
			Side: f.side, Qty: f.qty, Price: f.price, ExecTime: t0.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	open, err := q.ListOpenPositions(ctx, u.ID)
	if err != nil || len(open) != 1 || open[0].NetQty != 1 {
		t.Fatalf("open positions = %+v, err=%v", open, err)
	}

	h := &Handlers{DB: conn, Queries: q}
	uctx := auth.WithUser(ctx, &auth.User{ID: u.ID, Username: "trader"})
	rows := h.openPositionRows(uctx, open)
	if len(rows) != 1 || rows[0].AvgEntry == nil || *rows[0].AvgEntry != 6530 {
		t.Fatalf("rows = %+v (avg=%v)", rows, rows[0].AvgEntry)
	}
	if rows[0].LastPrice != nil || rows[0].Unrealized != nil {
		t.Errorf("without market data the row must carry no price/P&L: %+v", rows[0])
	}

	// With market data (bars only): last cached ES=F close 6545.25 -> 61 ticks x 12.50 x 1 contract.
	bar := time.Now().Add(-10 * time.Minute).Truncate(time.Minute)
	h.MarketData = &marketdata.CachingSource{DB: conn, Loc: time.UTC, Source: barsAt{t: bar, close: 6545.25}}
	rows = h.openPositionRows(uctx, open)
	if rows[0].LastPrice == nil || *rows[0].LastPrice != 6545.25 || rows[0].LastAt == nil || !rows[0].LastAt.Equal(bar) || rows[0].PriceSource != "Yahoo" {
		t.Fatalf("last price = %v @ %v via %q", rows[0].LastPrice, rows[0].LastAt, rows[0].PriceSource)
	}
	if rows[0].Unrealized == nil || *rows[0].Unrealized != 61*12.5 {
		t.Errorf("unrealized = %v, want %v", rows[0].Unrealized, 61*12.5)
	}

	// A source with a live quote wins over bars, and the quote is memoized:
	// swapping the source within the cache TTL does not change the answer.
	live := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	h.MarketData = &marketdata.CachingSource{DB: conn, Loc: time.UTC, Source: quoteAt{barsAt: barsAt{t: bar, close: 6545.25}, price: 6550, at: live}}
	rows = h.openPositionRows(uctx, open)
	if *rows[0].LastPrice != 6545.25 {
		t.Errorf("memoized quote should still be served: %v", *rows[0].LastPrice)
	}
	h.quoteCache.Delete("ESZ6")
	rows = h.openPositionRows(uctx, open)
	if rows[0].LastPrice == nil || *rows[0].LastPrice != 6550 || !rows[0].LastAt.Equal(live) || *rows[0].Unrealized != 80*12.5 {
		t.Errorf("live quote row = %+v", rows[0])
	}

	// Closing the last contract removes the position entirely.
	if _, err := q.InsertManualExecution(ctx, models.InsertManualExecutionParams{
		AccountID: acct.ID, BrokerExecID: "T-z", Symbol: "ESZ6", SymbolRoot: "ES",
		Side: "sell", Qty: 1, Price: 6540, ExecTime: t0.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if open, _ := q.ListOpenPositions(ctx, u.ID); len(open) != 0 {
		t.Errorf("flat account still listed: %+v", open)
	}
}

// barsAt is a BarSource that always knows exactly one bar (used to seed the
// market-data cache in tests without a vendor).
type barsAt struct {
	t     time.Time
	close float64
}

func (b barsAt) FetchBars(_ context.Context, _ string, _ marketdata.Timeframe, from, to time.Time) ([]marketdata.Bar, error) {
	if b.t.Unix() < from.Unix() || b.t.Unix() >= to.Unix() {
		return nil, marketdata.ErrNoData
	}
	return []marketdata.Bar{{Time: b.t.Unix(), Open: b.close, High: b.close, Low: b.close, Close: b.close, Volume: 1}}, nil
}

// quoteAt is barsAt plus a live quote (marketdata.QuoteSource).
type quoteAt struct {
	barsAt
	price float64
	at    time.Time
}

func (q quoteAt) Quote(context.Context, string) (marketdata.LatestQuote, error) {
	return marketdata.LatestQuote{Price: q.price, At: q.at}, nil
}

// Pending P&L follows the instrument's tick specs and the position's sign; a
// stub instrument (no tick specs) yields nothing rather than a guess.
func TestUnrealizedPnL(t *testing.T) {
	tick, val := 0.25, 12.5
	es := models.Instrument{SymbolRoot: "ES", TickSize: &tick, TickValue: &val}
	cases := []struct {
		name           string
		avg, last, qty float64
		inst           models.Instrument
		want           *float64
	}{
		{"long gains on rise", 6530, 6545.25, 2, es, f(1525)}, // 61 ticks x 12.5 x 2
		{"long loses on fall", 6530, 6525, 1, es, f(-250)},    // -20 ticks x 12.5
		{"short gains on fall", 6530, 6525, -2, es, f(500)},   // -20 ticks x 12.5 x -2
		{"short loses on rise", 6530, 6531, -1, es, f(-50)},   // 4 ticks x 12.5 x -1
		{"flat price", 6530, 6530, 3, es, f(0)},
		{"stub instrument", 6530, 6545, 1, models.Instrument{SymbolRoot: "MGC"}, nil},
		{"zero qty", 6530, 6545, 0, es, nil},
	}
	for _, c := range cases {
		got := unrealizedPnL(c.avg, c.last, c.qty, c.inst)
		switch {
		case (got == nil) != (c.want == nil):
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		case got != nil && *got != *c.want:
			t.Errorf("%s: got %g, want %g", c.name, *got, *c.want)
		}
	}
}

func f(v float64) *float64 { return &v }
