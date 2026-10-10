package positions

import (
	"math"

	"github.com/w4jnl/trading-journal/internal/models"
)

// PlannedRisk is the money at risk between the entry and a planned stop
// for qty contracts: the price distance times the instrument's value per
// point (tick_value / tick_size, else the multiplier). nil when the
// instrument has no specs yet (a stub awaiting setup), when the stop sits
// on the entry, or when either price is not positive. The sign of the
// distance is ignored: a stop on the wrong side of the entry is the
// trader's plan to record, not ours to refuse.
func PlannedRisk(entry, stop, qty float64, inst models.Instrument) *float64 {
	var mult float64
	switch {
	case inst.TickSize != nil && inst.TickValue != nil && *inst.TickSize > 0:
		mult = *inst.TickValue / *inst.TickSize
	case inst.Multiplier != nil:
		mult = *inst.Multiplier
	default:
		return nil
	}
	if entry <= 0 || stop <= 0 || qty <= 0 || mult <= 0 {
		return nil
	}
	risk := math.Abs(entry-stop) * mult * qty
	if risk == 0 {
		return nil
	}
	return &risk
}

// RMultiple is net P&L over planned risk, nil when either is missing or
// the risk is not positive.
func RMultiple(net, risk *float64) *float64 {
	if net == nil || risk == nil || *risk <= 0 {
		return nil
	}
	r := *net / *risk
	return &r
}
