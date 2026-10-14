// Package excursion measures how far price travelled while a trade was
// open: the maximum favourable excursion (MFE, the best the trade ever
// looked) and the maximum adverse excursion (MAE, the most heat it took),
// read off cached market-data bars. Exit efficiency, realized over MFE, is
// the share of the available move the trader actually captured.
package excursion

import (
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/positions"
)

// Excursion is a trade's price path summarised from the average entry, in
// price points, both never negative.
type Excursion struct {
	MAEPoints float64
	MFEPoints float64
	Timeframe marketdata.Timeframe // the bars it was read from
	Bars      int                  // how many bars overlapped the trade
}

// Compute reads the excursion off bars of timeframe tf. A bar counts when
// its span [open, open+width) overlaps the holding interval: it ends after
// the entry and opens no later than the exit. ok is false when no bar
// overlaps, or when the bars are too coarse to mean anything: one-minute
// bars always qualify (the finest the cache holds; a 40-second scalp still
// lives inside one), anything wider must fit the holding time at least
// once, else most of the bar's range is time the trade was not on and a
// three-minute scalp read off hourly bars would report the hour's range.
func Compute(direction string, entryPrice float64, entryTime, exitTime time.Time, bars []marketdata.Bar, tf marketdata.Timeframe) (Excursion, bool) {
	width := tf.Duration()
	if width <= 0 || entryPrice <= 0 || !exitTime.After(entryTime) {
		return Excursion{}, false
	}
	if tf != marketdata.TF1m && tf != marketdata.TF1s && exitTime.Sub(entryTime) < width {
		return Excursion{}, false
	}
	entry, exit := entryTime.Unix(), exitTime.Unix()
	w := int64(width / time.Second)
	hi, lo := entryPrice, entryPrice
	n := 0
	for _, b := range bars {
		if b.Time+w <= entry || b.Time > exit {
			continue
		}
		n++
		if b.High > hi {
			hi = b.High
		}
		if b.Low < lo {
			lo = b.Low
		}
	}
	if n == 0 {
		return Excursion{}, false
	}
	out := Excursion{Timeframe: tf, Bars: n}
	if direction == "short" {
		out.MFEPoints, out.MAEPoints = entryPrice-lo, hi-entryPrice
	} else {
		out.MFEPoints, out.MAEPoints = hi-entryPrice, lo2(entryPrice-lo)
	}
	out.MFEPoints, out.MAEPoints = lo2(out.MFEPoints), lo2(out.MAEPoints)
	return out, true
}

// lo2 clamps at zero: an excursion that never happened is zero, not negative.
func lo2(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// Realized is the points the trade made in its own direction (negative on
// a loss): exit minus entry for a long, entry minus exit for a short.
func Realized(direction string, entryPrice, exitPrice float64) float64 {
	if direction == "short" {
		return entryPrice - exitPrice
	}
	return exitPrice - entryPrice
}

// Efficiency is realized over MFE: 1.0 means the exit caught the very best
// price the trade saw, 0.5 half of it, negative a trade that had profit on
// the table and closed at a loss. nil when the trade never went in its
// favour (MFE 0), where the ratio has no meaning.
func Efficiency(direction string, entryPrice, exitPrice, mfePoints float64) *float64 {
	if mfePoints <= 0 {
		return nil
	}
	e := Realized(direction, entryPrice, exitPrice) / mfePoints
	return &e
}

// Money converts points for qty contracts to the account currency through
// the instrument's point value; nil on a stub instrument.
func Money(points, qty float64, inst models.Instrument) *float64 {
	mult, ok := positions.PointValue(inst)
	if !ok || qty <= 0 {
		return nil
	}
	v := points * mult * qty
	return &v
}
