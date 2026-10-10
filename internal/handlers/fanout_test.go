package handlers

import (
	"testing"
	"time"
)

func TestGroupLinkedTradesFoldsFanOut(t *testing.T) {
	t0 := time.Date(2026, 7, 31, 13, 33, 10, 0, time.UTC)
	mk := func(acct, sym, root, dir string, qty float64, at time.Time, net *float64) linkedTrade {
		exit := at.Add(5 * time.Minute)
		return linkedTrade{ID: int64(len(acct)), Symbol: sym, SymbolRoot: root, Direction: dir, Qty: qty, AccountName: acct, EntryTime: at, ExitTime: &exit, NetPnl: net}
	}
	trades := []linkedTrade{
		// One decision fanned to three accounts within two seconds; one is a
		// micro on the full-size root and one trades the next contract month.
		mk("PRO002", "MESU6", "MES", "short", 2, t0, ptrF(6.75)),
		mk("PRO003", "MESU6", "MES", "short", 2, t0.Add(time.Second), ptrF(6.75)),
		mk("U10000001", "MESU26", "MES", "short", 4, t0.Add(2*time.Second), ptrF(12.62)),
		// Same root/direction but 10 s later: a separate decision.
		mk("PRO002", "MESU6", "MES", "short", 1, t0.Add(10*time.Second), ptrF(10.03)),
		// Opposite direction at the same second: separate.
		mk("PRO002", "MESU6", "MES", "long", 1, t0, ptrF(-3)),
		// A single account scaling in twice within two seconds is NOT fan-out.
		mk("SOLO", "NQU6", "NQ", "long", 1, t0, ptrF(20)),
		mk("SOLO", "NQU6", "NQ", "long", 1, t0.Add(time.Second), ptrF(25)),
	}
	groups := groupLinkedTrades(trades)
	if len(groups) != 5 {
		for _, g := range groups {
			t.Logf("group lead=%s %s acc=%d n=%d", g.Lead.AccountName, g.Lead.Direction, g.Accounts, len(g.Members))
		}
		t.Fatalf("groups = %d, want 5 (fan-out, later short, long, two solo rows)", len(groups))
	}
	var fan *linkedTradeGroup
	for i := range groups {
		if groups[i].Accounts == 3 {
			fan = &groups[i]
		}
	}
	if fan == nil {
		t.Fatal("no three-account group")
	}
	if fan.Qty != 8 || fan.Net == nil || *fan.Net < 26.119 || *fan.Net > 26.121 {
		t.Errorf("fan-out qty/net = %v / %v, want 8 / 26.12", fan.Qty, fan.Net)
	}
	if fan.Symbols != "MESU26, MESU6" {
		t.Errorf("fan-out symbols = %q, want both contracts listed", fan.Symbols)
	}
	// Display order follows the lead entry time.
	for i := 1; i < len(groups); i++ {
		if groups[i].Lead.EntryTime.Before(groups[i-1].Lead.EntryTime) {
			t.Errorf("groups out of time order at %d", i)
		}
	}
	// A NULL net anywhere in the group leaves the group net NULL.
	trades[1].NetPnl = nil
	for _, g := range groupLinkedTrades(trades) {
		if g.Accounts == 3 && g.Net != nil {
			t.Errorf("group net = %v with a NULL member, want nil", *g.Net)
		}
	}
	if groupLinkedTrades(nil) != nil {
		t.Error("nil in, nil out")
	}
}

func TestFoldMarkersMergesIdenticalFills(t *testing.T) {
	ms := []chartMarker{
		{T: 100, Price: 7510.25, Side: "sell", Role: "entry", Qty: 1},
		{T: 90, Price: 7500, Side: "buy", Role: "exit", Qty: 1},
		{T: 100, Price: 7510.25, Side: "sell", Role: "entry", Qty: 2},
		{T: 100, Price: 7510.25, Side: "sell", Role: "entry", Qty: 1},
		{T: 100, Price: 7510.25, Side: "sell", Role: "exit", Qty: 1}, // same point, different role: kept apart
	}
	out := foldMarkers(ms)
	if len(out) != 3 {
		t.Fatalf("folded = %+v, want 3 markers", out)
	}
	if out[0].T != 90 {
		t.Errorf("not sorted by time: %+v", out)
	}
	var entry *chartMarker
	for i := range out {
		if out[i].Role == "entry" && out[i].T == 100 {
			entry = &out[i]
		}
	}
	if entry == nil || entry.Count != 3 || entry.Qty != 4 {
		t.Errorf("folded entry = %+v, want count 3 qty 4", entry)
	}
	for _, m := range out {
		if m.Count == 1 {
			t.Errorf("a lone marker carries count 1 (should be 0/omitted): %+v", m)
		}
	}
}
