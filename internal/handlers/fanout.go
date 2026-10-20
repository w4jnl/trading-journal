package handlers

import (
	"sort"
)

// Copy sets in the journal trade tables. One decision filled on a dozen
// follower accounts produces a dozen near-identical rows; when the trader
// reads the log by decision (the Trades | Sets toggle), groupLinkedTrades
// folds the members of each copy set (internal/copyset, trade_set_members)
// into one line per decision, with the members available for expansion.
// A trade in no set is a row of its own.

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
	var groups []linkedTradeGroup
	bySet := map[int64]int{}
	for _, t := range trades {
		if t.SetID == nil {
			groups = append(groups, linkedTradeGroup{Lead: t, Members: []linkedTrade{t}})
			continue
		}
		i, ok := bySet[*t.SetID]
		if !ok {
			i = len(groups)
			bySet[*t.SetID] = i
			groups = append(groups, linkedTradeGroup{Lead: t})
		}
		g := &groups[i]
		g.Members = append(g.Members, t)
		if t.EntryTime.Before(g.Lead.EntryTime) {
			g.Lead = t
		}
	}
	for i := range groups {
		g := &groups[i]
		sort.SliceStable(g.Members, func(a, b int) bool { return g.Members[a].EntryTime.Before(g.Members[b].EntryTime) })
		seen := map[string]bool{}
		syms := map[string]bool{}
		var net float64
		netOK := true
		for _, m := range g.Members {
			seen[m.AccountName] = true
			syms[m.Symbol] = true
			g.Qty += m.Qty
			if m.NetPnl == nil {
				netOK = false
			} else {
				net += *m.NetPnl
			}
		}
		g.Accounts = len(seen)
		if netOK {
			g.Net = &net
		}
		if len(syms) > 1 {
			names := make([]string, 0, len(syms))
			for s := range syms {
				names = append(names, s)
			}
			sort.Strings(names)
			g.Symbols = joinComma(names)
		}
	}
	// Time order for display, by the lead's entry.
	sort.SliceStable(groups, func(a, b int) bool { return groups[a].Lead.EntryTime.Before(groups[b].Lead.EntryTime) })
	return groups
}

// summarizeGroups is the summary card by decision: one count per group,
// wins and losses by the group's net, the money over every member.
func summarizeGroups(groups []linkedTradeGroup) GroupSummary {
	var s GroupSummary
	for _, g := range groups {
		s.N++
		for _, t := range g.Members {
			s.Trades++
			if t.GrossPnl != nil {
				s.Gross += *t.GrossPnl
			}
			if t.Commission != nil {
				s.Commission += *t.Commission
			}
			if t.NetPnl != nil {
				s.Net += *t.NetPnl
			}
			if t.ExitTime != nil && t.ExitTime.After(s.LastExit) {
				s.LastExit = *t.ExitTime
			}
		}
		if g.Net != nil {
			switch {
			case *g.Net > 0:
				s.Wins++
			case *g.Net < 0:
				s.Losses++
			}
		}
	}
	return s
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
