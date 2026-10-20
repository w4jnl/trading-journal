package handlers

import (
	"testing"
	"time"
)

func TestGroupLinkedTradesFoldsSets(t *testing.T) {
	t0 := time.Date(2026, 7, 31, 13, 33, 10, 0, time.UTC)
	set := func(id int64) *int64 { return &id }
	mk := func(acct, sym, root, dir string, qty float64, at time.Time, net *float64, setID *int64) linkedTrade {
		exit := at.Add(5 * time.Minute)
		return linkedTrade{ID: int64(len(acct)), Symbol: sym, SymbolRoot: root, Direction: dir, Qty: qty, AccountName: acct, EntryTime: at, ExitTime: &exit, NetPnl: net, SetID: setID}
	}
	trades := []linkedTrade{
		// One decision on three accounts (set 7), given out of entry order; one
		// leg trades the next contract month.
		mk("PRO003", "MESU6", "MES", "short", 2, t0.Add(time.Second), ptrF(6.75), set(7)),
		mk("PRO002", "MESU6", "MES", "short", 2, t0, ptrF(6.75), set(7)),
		mk("U10000001", "MESU26", "MES", "short", 4, t0.Add(2*time.Second), ptrF(12.62), set(7)),
		// A later short on one account: its own row.
		mk("PRO002", "MESU6", "MES", "short", 1, t0.Add(10*time.Second), ptrF(10.03), nil),
		// A second set, two accounts, a loser.
		mk("PRO002", "NQU6", "NQ", "long", 1, t0.Add(20*time.Second), ptrF(-3), set(9)),
		mk("PRO003", "NQU6", "NQ", "long", 1, t0.Add(21*time.Second), ptrF(-4), set(9)),
	}
	groups := groupLinkedTrades(trades)
	if len(groups) != 3 {
		for _, g := range groups {
			t.Logf("group lead=%s %s acc=%d n=%d", g.Lead.AccountName, g.Lead.Direction, g.Accounts, len(g.Members))
		}
		t.Fatalf("groups = %d, want 3 (the set, the lone short, the second set)", len(groups))
	}
	fan := groups[0]
	if fan.Accounts != 3 || fan.Lead.AccountName != "PRO002" || fan.Members[0].AccountName != "PRO002" || fan.Members[2].AccountName != "U10000001" {
		t.Errorf("set group = %+v, want the earliest leg leading and members in entry order", fan)
	}
	if fan.Qty != 8 || fan.Net == nil || *fan.Net < 26.119 || *fan.Net > 26.121 {
		t.Errorf("set qty/net = %v / %v, want 8 / 26.12", fan.Qty, fan.Net)
	}
	if fan.Symbols != "MESU26, MESU6" {
		t.Errorf("set symbols = %q, want both contracts listed", fan.Symbols)
	}
	if groups[1].Accounts != 1 || groups[1].Lead.Qty != 1 || groups[2].Accounts != 2 {
		t.Errorf("groups = %+v", groups)
	}
	// Display order follows the lead entry time.
	for i := 1; i < len(groups); i++ {
		if groups[i].Lead.EntryTime.Before(groups[i-1].Lead.EntryTime) {
			t.Errorf("groups out of time order at %d", i)
		}
	}
	// The summary by decision: three rows in six trades, two winners and a loser.
	if s := summarizeGroups(groups); s.N != 3 || s.Trades != 6 || s.Wins != 2 || s.Losses != 1 || s.Net < 29.149 || s.Net > 29.151 {
		t.Errorf("summary by decision = %+v", s)
	}
	if s := summarizeTradeRows(trades); s.N != 6 || s.Trades != 6 || s.Wins != 4 || s.Losses != 2 {
		t.Errorf("summary by trade = %+v", s)
	}
	// A NULL net anywhere in the group leaves the group net NULL and the
	// decision uncounted as a win or a loss.
	trades[1].NetPnl = nil
	g2 := groupLinkedTrades(trades)
	if g2[0].Net != nil {
		t.Errorf("group net = %v with a NULL member, want nil", *g2[0].Net)
	}
	if s := summarizeGroups(g2); s.Wins != 1 {
		t.Errorf("wins with a NULL leg = %d, want 1", s.Wins)
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
