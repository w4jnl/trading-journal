package handlers

import (
	"sort"
	"time"
)

// Copy-trader fan-out in the journal trade tables. One decision filled on a
// dozen follower accounts produces a dozen near-identical rows (same root,
// direction and second); the 31 July day page listed 35 rows for about
// five decisions. groupLinkedTrades folds such rows into one line per
// decision, with the members available for expansion. The clustering rule
// mirrors prepare-day's clusterCopytrader: same copy key (micros fold into
// their full-size root) and direction, entries within two seconds, at
// least two distinct accounts.

const fanoutWindow = 2 * time.Second

// linkedTradeGroup is one row of the folded table. Accounts is the number
// of distinct accounts; a group of one renders as a plain row.
type linkedTradeGroup struct {
	Lead     linkedTrade
	Members  []linkedTrade
	Accounts int
	Qty      float64
	Net      *float64 // nil when any member's net is NULL (stub instrument)
	Symbols  string   // distinct contracts when they differ across members ("MESU6, MESU26")
}

func groupLinkedTrades(trades []linkedTrade) []linkedTradeGroup {
	if len(trades) == 0 {
		return nil
	}
	idx := make([]int, len(trades))
	for i := range idx {
		idx[i] = i
	}
	key := func(t linkedTrade) string { return copyClusterKey(t.SymbolRoot) + "|" + t.Direction }
	sort.SliceStable(idx, func(a, b int) bool {
		ta, tb := trades[idx[a]], trades[idx[b]]
		if ka, kb := key(ta), key(tb); ka != kb {
			return ka < kb
		}
		return ta.EntryTime.Before(tb.EntryTime)
	})
	var groups []linkedTradeGroup
	var cur *linkedTradeGroup
	var curKey string
	var curStart time.Time
	flush := func() {
		if cur == nil {
			return
		}
		seen := map[string]bool{}
		var net float64
		netOK := true
		syms := map[string]bool{}
		for _, m := range cur.Members {
			seen[m.AccountName] = true
			syms[m.Symbol] = true
			cur.Qty += m.Qty
			if m.NetPnl == nil {
				netOK = false
			} else {
				net += *m.NetPnl
			}
		}
		cur.Accounts = len(seen)
		if netOK {
			cur.Net = &net
		}
		if len(syms) > 1 {
			names := make([]string, 0, len(syms))
			for s := range syms {
				names = append(names, s)
			}
			sort.Strings(names)
			cur.Symbols = joinComma(names)
		}
		groups = append(groups, *cur)
		cur = nil
	}
	for _, i := range idx {
		t := trades[i]
		k := key(t)
		if cur == nil || k != curKey || t.EntryTime.Sub(curStart) > fanoutWindow {
			flush()
			cur = &linkedTradeGroup{Lead: t}
			curKey, curStart = k, t.EntryTime
		}
		cur.Members = append(cur.Members, t)
	}
	flush()
	// Back to time order for display (groups by their lead's entry).
	sort.SliceStable(groups, func(a, b int) bool { return groups[a].Lead.EntryTime.Before(groups[b].Lead.EntryTime) })
	// A cluster on a single account is not fan-out: split it back into rows.
	var out []linkedTradeGroup
	for _, g := range groups {
		if g.Accounts > 1 {
			out = append(out, g)
			continue
		}
		for _, m := range g.Members {
			out = append(out, linkedTradeGroup{Lead: m, Members: []linkedTrade{m}, Accounts: 1, Qty: m.Qty, Net: m.NetPnl})
		}
	}
	return out
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
