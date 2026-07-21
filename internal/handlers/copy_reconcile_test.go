package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// TestCompareCopyAccounts verifies the gross-volume comparison catches a copy
// divergence even when both accounts end flat (net 0) - the case the old
// net-position comparison was blind to - and does not false-flag an exact
// mirror.
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
	// Leader trades a full round turn: buy 2, sell 2 (ends flat).
	ins(leader, "L1", "buy", 2, 0)
	ins(leader, "L2", "sell", 2, 30)
	// Follower (2x): buy only 1 instead of 4 (under-fill), but sells the full
	// 4. Both accounts still end FLAT-ish on net, yet the gross buy volume is
	// short: exactly the case net comparison missed.
	ins(follower, "F1", "buy", 1, 0)
	ins(follower, "F2", "sell", 4, 30)

	lines, err := h.compareCopyAccounts(ctx, leader, follower, 2, nil, nil)
	if err != nil {
		t.Fatalf("compareCopyAccounts: %v", err)
	}
	byKey := map[string]copyLine{}
	for _, l := range lines {
		byKey[l.Symbol+"/"+l.Side] = l
	}
	buy := byKey["MESU6/buy"]
	if !buy.Diverged || buy.Leader != 2 || buy.Expected != 4 || buy.Follower != 1 || buy.Drift != -3 {
		t.Errorf("buy line = %+v, want leader 2 / expected 4 / follower 1 / drift -3 / diverged", buy)
	}
	if len(buy.FollowerFills) != 1 || buy.FollowerFills[0].Ref != "F1" || buy.FollowerFills[0].Cum != 1 {
		t.Errorf("buy follower fills = %+v, want the single F1 under-fill with cum 1", buy.FollowerFills)
	}
	sell := byKey["MESU6/sell"]
	if sell.Diverged {
		t.Errorf("sell line should match (leader 2 x2 = 4 = follower 4): %+v", sell)
	}

	// Date filter excludes the fills -> nothing to compare.
	nextDay := base.AddDate(0, 0, 1)
	empty, _ := h.compareCopyAccounts(ctx, leader, follower, 2, &nextDay, nil)
	if len(empty) != 0 {
		t.Errorf("out-of-range comparison should be empty, got %+v", empty)
	}

	// Exact 2x mirror on both sides -> no divergence.
	good := mk("GOOD")
	ins(good, "G1", "buy", 4, 0)
	ins(good, "G2", "sell", 4, 30)
	glines, _ := h.compareCopyAccounts(ctx, leader, good, 2, nil, nil)
	for _, l := range glines {
		if l.Diverged {
			t.Errorf("exact 2x follower should not diverge: %+v", l)
		}
	}
}
