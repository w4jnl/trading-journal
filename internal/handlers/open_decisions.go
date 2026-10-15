package handlers

import (
	"math"
	"sort"
)

// ---- The open strip in the sets view ------------------------------------------
//
// The /trades sets view shows one row per decision; the open strip above
// it does the same: a copied decision's legs (positions.ClusterOpen, as
// linkOpenSets linked them) fold into one row with their accounts under
// a chevron, like a closed set, and a position on its own is a row of its
// own. The decision row sums what adds up (size, risk, unrealized), averages
// the entry with the spread between the legs, and shows the stop and
// target only when every planned leg has the same ones.

// openDecision is one row of the strip in the sets view.
type openDecision struct {
	// Key is "posset:<account>:<symbol>" of the lead leg: trade_sheet.js
	// opens that leg's sheet (which lists the others), and the key stays
	// distinct from the lead's own member row.
	Key      string
	Lead     positionView
	Members  []positionView // every leg, the lead first; nil for a position on its own
	IsSet    bool
	Accounts int
	Qty      float64
	EntryAvg *float64 // size-weighted over the legs; nil when a leg has no entry
	Spread   float64  // between the best and the worst leg's entry
	// Unrealized and Risk sum the legs that have them; nil when none has.
	Unrealized *float64
	Risk       *float64
	// Stop and Target are the levels every planned leg shares; nil when
	// none is planned or the legs differ (Varies).
	Stop       *float64
	Target     *float64
	Varies     bool
	HasPlan    bool // any leg has a plan
	Unplanned  int  // legs without a plan
	BeyondStop bool // any leg's last price is through its stop
}

// groupOpenDecisions folds the open positions into decisions, in the order
// their lead legs appear.
func groupOpenDecisions(views []positionView) []openDecision {
	var out []openDecision
	for _, v := range views {
		if !leadsDecision(v, false) {
			continue
		}
		d := openDecision{Key: "posset:" + v.Key[len("pos:"):], Lead: v}
		if v.InSet() {
			d.IsSet = true
			d.Members = append([]positionView{v}, v.Legs...)
			for i := range d.Members {
				d.Members[i].Legs = nil // under the decision row, the badge says nothing new
			}
			sort.SliceStable(d.Members, func(i, j int) bool {
				a, b := d.Members[i], d.Members[j]
				if !a.OpenedAt.Equal(b.OpenedAt) {
					return a.OpenedAt.Before(b.OpenedAt)
				}
				return a.AccountID < b.AccountID
			})
		}
		legs := d.Members
		if legs == nil {
			legs = []positionView{v}
		}
		d.Accounts = len(legs)
		var weighted, unreal, risk float64
		var lo, hi float64
		entries, unrealN, riskN := 0, 0, 0
		var stop, target *float64
		for _, l := range legs {
			d.Qty += l.Qty
			if l.AvgEntry != nil {
				weighted += *l.AvgEntry * l.Qty
				if entries == 0 || *l.AvgEntry < lo {
					lo = *l.AvgEntry
				}
				if entries == 0 || *l.AvgEntry > hi {
					hi = *l.AvgEntry
				}
				entries++
			}
			if l.Unrealized != nil {
				unreal += *l.Unrealized
				unrealN++
			}
			if l.Risk != nil {
				risk += *l.Risk
				riskN++
			}
			if l.BeyondStop {
				d.BeyondStop = true
			}
			if !l.HasPlan() {
				d.Unplanned++
				continue
			}
			if !d.HasPlan {
				d.HasPlan, stop, target = true, l.Stop, l.Target
				continue
			}
			if !sameFloat(stop, l.Stop) || !sameFloat(target, l.Target) {
				d.Varies = true
			}
		}
		if entries == len(legs) && d.Qty > 0 {
			avg := weighted / d.Qty
			d.EntryAvg = &avg
			d.Spread = math.Round((hi-lo)*100) / 100
		}
		if unrealN > 0 {
			d.Unrealized = &unreal
		}
		if riskN > 0 {
			d.Risk = &risk
		}
		if d.HasPlan && !d.Varies {
			d.Stop, d.Target = stop, target
		}
		out = append(out, d)
	}
	return out
}
