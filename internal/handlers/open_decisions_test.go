package handlers

import (
	"testing"
	"time"
)

// Legs fold into one decision led by the earliest leg, with the size and
// risk summed, the entry averaged with its spread, and the stop shown only
// while every planned leg shares it; a position on its own stays a row.
func TestGroupOpenDecisions(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 14, 30, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	mk := func(acct int64, sym string, at time.Time, qty, entry float64, stop, risk *float64) positionView {
		v := positionView{Key: positionKey(acct, sym), Direction: "long", Qty: qty, AvgEntry: &entry, OpenedAt: at, Stop: stop, Risk: risk}
		v.AccountID, v.Symbol = acct, sym
		return v
	}
	a := mk(1, "ESM6", t0, 2, 7250, f(7241), f(900))
	b := mk(2, "MESM6", t0.Add(time.Second), 4, 7250.5, f(7241), f(190))
	c := mk(3, "CLZ6", t0.Add(time.Hour), 1, 80, nil, nil)
	a.Legs, b.Legs = []positionView{b}, []positionView{a}
	// b is listed first: the order of the views does not pick the lead.
	ds := groupOpenDecisions([]positionView{b, a, c})
	if len(ds) != 2 || !ds[0].IsSet || ds[1].IsSet {
		t.Fatalf("decisions = %+v", ds)
	}
	d := ds[0]
	if d.Key != "posset:1:ESM6" || d.Lead.AccountID != 1 || len(d.Members) != 2 || d.Members[0].AccountID != 1 || d.Accounts != 2 {
		t.Errorf("lead/members = %s %d %d", d.Key, d.Lead.AccountID, len(d.Members))
	}
	if d.Qty != 6 || d.EntryAvg == nil || *d.EntryAvg < 7250.33 || *d.EntryAvg > 7250.34 || d.Spread != 0.5 {
		t.Errorf("size %g entry %v spread %g", d.Qty, d.EntryAvg, d.Spread)
	}
	if d.Risk == nil || *d.Risk != 1090 || d.Stop == nil || *d.Stop != 7241 || d.Varies || !d.HasPlan || d.Unplanned != 0 {
		t.Errorf("plan = risk %v stop %v varies %v", d.Risk, d.Stop, d.Varies)
	}
	if ds[1].Key != "posset:3:CLZ6" || ds[1].Members != nil || ds[1].HasPlan || ds[1].Unplanned != 1 {
		t.Errorf("single = %+v", ds[1])
	}
	// A leg with a different stop: the shared levels go, the risk still sums.
	b.Stop = f(7243)
	a.Legs = []positionView{b}
	d = groupOpenDecisions([]positionView{a, b})[0]
	if !d.Varies || d.Stop != nil || d.Risk == nil || *d.Risk != 1090 {
		t.Errorf("differing stops = %+v", d)
	}
}
