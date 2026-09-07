package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Open-position rows link where the viewer may actually go: admins to the
// reconcile editor, everyone else to the account's trades for that
// instrument (the editor is behind RequireAdmin and would answer "admin
// only"). Data fields stay promoted for the partial.
func TestOpenPositionRowsLinkPerViewer(t *testing.T) {
	rows := []models.ListOpenPositionsRow{{AccountID: 16, AccountName: "U10000001", Symbol: "ESU6", SymbolRoot: "ES", NetQty: 1}}

	admin := auth.WithUser(context.Background(), &auth.User{ID: 1, Username: "a", IsAdmin: true})
	got := (&Handlers{}).openPositionRows(admin, rows)
	if len(got) != 1 || got[0].Href != "/admin/transactions?account_id=16&symbol=ESU6" || got[0].AccountName != "U10000001" {
		t.Errorf("admin row = %+v", got)
	}

	user := auth.WithUser(context.Background(), &auth.User{ID: 2, Username: "u"})
	got = (&Handlers{}).openPositionRows(user, rows)
	if len(got) != 1 || got[0].Href != "/trades?filter=acc%3A16&symbol_root=ES" {
		t.Errorf("non-admin row = %+v", got)
	}
	if strings.Contains(got[0].Href, "/admin/") {
		t.Errorf("non-admin must never be sent to an admin-only page")
	}
	// No user in context (defensive): the non-admin link.
	if got := (&Handlers{}).openPositionRows(context.Background(), rows); len(got) != 1 || strings.Contains(got[0].Href, "/admin/") {
		t.Errorf("anonymous row = %+v", got)
	}
	// Empty input stays an empty, non-nil slice so the partial renders "flat".
	if got := (&Handlers{}).openPositionRows(user, nil); got == nil || len(got) != 0 {
		t.Errorf("empty = %#v", got)
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
	rows := h.openPositionRows(auth.WithUser(ctx, &auth.User{ID: u.ID, Username: "trader"}), open)
	if len(rows) != 1 || rows[0].AvgEntry == nil || *rows[0].AvgEntry != 6530 {
		t.Fatalf("rows = %+v (avg=%v)", rows, rows[0].AvgEntry)
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
