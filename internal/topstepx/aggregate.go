package topstepx

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"time"
)

// OrderFill is one order's worth of half-turn trades collapsed into a single
// fill — the same order-level granularity the TopstepX orders CSV delivers,
// which keeps broker_exec_id (= the orderId) dedup-compatible between CSV
// and API imports.
type OrderFill struct {
	BrokerExecID string  // orderId as decimal string; "T<halfTradeId>" fallback when orderId is 0
	ContractID   string  // ProjectX contract id, resolved to a symbol by the caller
	Side         string  // "buy" | "sell"
	Qty          float64 // sum of half-trade sizes
	Price        float64 // size-weighted average price
	Commission   float64 // sum of fees + commissions across the halves
	ExecTime     time.Time
	RawData      string // compact JSON of the constituent half-trades, for audit
}

// aggKey groups half-trades. Side is included defensively: one order should
// only ever fill on one side, but merging opposite sides would corrupt FIFO.
type aggKey struct {
	orderKey string
	side     string
}

// AggregateOrders collapses half-turn trades into per-order fills. Voided
// halves are dropped. Within a group: qty = sum(size), price = size-weighted
// average, time = earliest half, commission = sum(fees + commissions-or-0).
// Output is sorted by exec time (then id) for deterministic ingestion.
func AggregateOrders(trades []HalfTrade, logger *slog.Logger) []OrderFill {
	if logger == nil {
		logger = slog.Default()
	}
	type accum struct {
		fill   OrderFill
		sumPxQ float64 // price*qty accumulator for the weighted average
		halves []map[string]any
	}
	groups := map[aggKey]*accum{}
	var order []aggKey

	for _, ht := range trades {
		if ht.Voided {
			continue
		}
		if ht.Size <= 0 {
			logger.Warn("topstepx: half-trade with non-positive size skipped", "id", ht.ID, "size", ht.Size)
			continue
		}
		side := "buy"
		if ht.Side == 1 { // 0 = Bid (buy), 1 = Ask (sell)
			side = "sell"
		}
		orderKey := strconv.FormatInt(ht.OrderID, 10)
		if ht.OrderID == 0 {
			// Defensive: a zero orderId would merge unrelated fills under one
			// key. Fall back to the half-trade's own id; the T prefix keeps it
			// out of the numeric orderId namespace.
			orderKey = "T" + strconv.FormatInt(ht.ID, 10)
			logger.Warn("topstepx: half-trade without orderId, using own id", "id", ht.ID)
		}
		k := aggKey{orderKey: orderKey, side: side}
		g, exists := groups[k]
		if !exists {
			g = &accum{fill: OrderFill{
				BrokerExecID: orderKey,
				ContractID:   ht.ContractID,
				Side:         side,
				ExecTime:     ht.CreationTimestamp,
			}}
			groups[k] = g
			order = append(order, k)
		}
		qty := float64(ht.Size)
		g.fill.Qty += qty
		g.sumPxQ += ht.Price * qty
		comm := ht.Fees
		if ht.Commissions != nil {
			comm += *ht.Commissions
		}
		g.fill.Commission += comm
		if ht.CreationTimestamp.Before(g.fill.ExecTime) {
			g.fill.ExecTime = ht.CreationTimestamp
		}
		g.halves = append(g.halves, map[string]any{
			"id": ht.ID, "price": ht.Price, "size": ht.Size, "fees": ht.Fees,
		})
	}

	fills := make([]OrderFill, 0, len(order))
	for _, k := range order {
		g := groups[k]
		g.fill.Price = g.sumPxQ / g.fill.Qty
		if raw, err := json.Marshal(g.halves); err == nil {
			g.fill.RawData = string(raw)
		}
		fills = append(fills, g.fill)
	}
	sort.Slice(fills, func(i, j int) bool {
		if !fills[i].ExecTime.Equal(fills[j].ExecTime) {
			return fills[i].ExecTime.Before(fills[j].ExecTime)
		}
		return fills[i].BrokerExecID < fills[j].BrokerExecID
	})
	return fills
}
