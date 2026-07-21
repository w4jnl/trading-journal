package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// TestCompareCopyAccounts reproduces the copy-trade divergence hunt: a 2x
// follower whose buy under-fills (leader bought 2 -> follower should hold 4
// but only got 1) shows a -3 drift, flagged on the exact fill; a follower
// that mirrors exactly shows none.
func TestCompareCopyAccounts(t *testing.T) {
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")
	h := &Handlers{Queries: q, Timezone: time.UTC}
	ctx := context.Background()

	firm := "apex"
	mk := func(ref string) int64 {
		a, err := q.CreateAccount(ctx, models.CreateAccountParams{
			UserID: uid, Name: ref, Firm: &firm, ImportSource: "motivewave_csv",
			AccountRef: ref, Currency: "USD", CommissionSource: "rate",
		})
		if err != nil {
			t.Fatalf("CreateAccount %s: %v", ref, err)
		}
		return a.ID
	}
	leader := mk("LEADER")
	follower := mk("FOLLOWER")

	base := time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	ins := func(acct int64, id, side string, qty float64, min int) {
		if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{
			AccountID: acct, BrokerExecID: id, Symbol: "MESU6", SymbolRoot: "MES",
			Side: side, Qty: qty, Price: 7500, ExecTime: base.Add(time.Duration(min) * time.Minute),
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// Leader: buy 2, later sell 2 (net 0).
	ins(leader, "L1", "buy", 2, 0)
	ins(leader, "L2", "sell", 2, 30)
	// Follower (2x): buy only 1 instead of 4 (under-fill), then sell 4.
	ins(follower, "F1", "buy", 1, 0)
	ins(follower, "F2", "sell", 4, 30)

	syms, err := h.compareCopyAccounts(ctx, leader, follower, 2)
	if err != nil {
		t.Fatalf("compareCopyAccounts: %v", err)
	}
	if len(syms) != 1 || syms[0].Symbol != "MESU6" {
		t.Fatalf("symbols = %+v", syms)
	}
	s := syms[0]
	if !s.Diverged || s.Drift != -3 {
		t.Errorf("drift = %v diverged=%v, want -3 / true", s.Drift, s.Diverged)
	}
	if s.LeaderNet != 0 || s.FollowerNet != -3 || s.Expected != 0 {
		t.Errorf("nets: leader=%v follower=%v expected=%v", s.LeaderNet, s.FollowerNet, s.Expected)
	}
	// The timeline is trimmed to the divergence; the under-buy (F1) drives it.
	var flagged bool
	for _, r := range s.Rows {
		if r.Ref == "F1" && r.Drift != 0 {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("expected the under-filled follower buy F1 flagged with nonzero drift; rows=%+v", s.Rows)
	}

	// A follower that mirrors exactly diverges on nothing.
	good := mk("GOOD")
	ins(good, "G1", "buy", 4, 0)
	ins(good, "G2", "sell", 4, 30)
	syms2, err := h.compareCopyAccounts(ctx, leader, good, 2)
	if err != nil {
		t.Fatalf("compare good: %v", err)
	}
	if len(syms2) != 1 || syms2[0].Diverged {
		t.Errorf("exact 2x follower should not diverge, got %+v", syms2)
	}
	if len(syms2[0].Rows) != 0 {
		t.Errorf("aligned symbol should have no trimmed detail rows, got %d", len(syms2[0].Rows))
	}
}
