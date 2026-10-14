package positions

import "github.com/w4jnl/trading-journal/internal/models"

// OpenPosition is what remains after replaying an (account, symbol) fill
// history: the signed net quantity (+ long, - short) and the volume-weighted
// average price of the lots still open. Flat -> zero values.
//
// Opened identifies the cycle: the fill that took the account from flat
// (or the flip fill whose remainder opened the other way), the same fill
// Builder.replay records as the trade's first entry, so a plan anchored on
// it meets the trade the cycle becomes. Fills is how many fills the cycle
// holds so far (entries and partial exits).
type OpenPosition struct {
	NetQty   float64
	AvgPrice float64
	Opened   models.Execution
	Fills    int
}

// OpenFromFills replays fills in the given (chronological) order with the
// same FIFO rule Builder.replay uses, so the open position's average entry
// agrees with how the closed trades were carved out: a same-direction fill
// adds a lot; an opposite-direction fill consumes from the oldest lot first;
// a fill larger than the open quantity closes everything and its remainder
// opens a new position the other way. Only the surviving lots enter the
// average, which is what makes it right after partial closes - a naive
// "average of all buys" would still count contracts already sold.
//
// The caller supplies live fills only (soft-deleted rows excluded), ordered by
// exec_time then id, as ListExecutionsForAccountSymbol returns them.
func OpenFromFills(fills []models.Execution) OpenPosition {
	type lot struct{ qty, price float64 }
	var lots []lot
	long := true
	var opened models.Execution
	count := 0
	for _, f := range fills {
		if f.Qty <= 0 {
			continue
		}
		fillLong := f.Side != "sell"
		if len(lots) == 0 {
			long = fillLong
			lots = []lot{{qty: f.Qty, price: f.Price}}
			opened, count = f, 1
			continue
		}
		count++
		if fillLong == long {
			lots = append(lots, lot{qty: f.Qty, price: f.Price})
			continue
		}
		left := f.Qty
		for left > 0 && len(lots) > 0 {
			head := &lots[0]
			take := head.qty
			if left < take {
				take = left
			}
			head.qty -= take
			left -= take
			if head.qty <= 0 {
				lots = lots[1:]
			}
		}
		if left > 0 { // flip
			long = fillLong
			lots = []lot{{qty: left, price: f.Price}}
			opened, count = f, 1
		}
	}
	var qty, notional float64
	for _, l := range lots {
		qty += l.qty
		notional += l.qty * l.price
	}
	if qty == 0 {
		return OpenPosition{}
	}
	pos := OpenPosition{NetQty: qty, AvgPrice: notional / qty, Opened: opened, Fills: count}
	if !long {
		pos.NetQty = -qty
	}
	return pos
}
