// Package marketdata fetches and caches historical OHLCV price bars for the
// trade-replay charts. The only v1 source is Yahoo Finance v8 (free, no key),
// behind the BarSource interface so an exact-contract source (IBKR, Databento)
// can be added later without touching the cache or handler layers.
//
// Bars are immutable historical data keyed by (yahoo_symbol, timeframe,
// bar_time). The cache stores the finest timeframe a day's age allows and the
// handler rolls it up to a coarser DISPLAY timeframe at render time, so what is
// stored is decoupled from what is drawn.
package marketdata

import (
	"context"
	"errors"
	"time"
)

// Timeframe is a bar interval. The finer intraday frames (15m/30m) are display
// roll-up targets only; StorageTimeframe never persists at those.
type Timeframe string

const (
	TF1s  Timeframe = "1s"
	TF5s  Timeframe = "5s"
	TF15s Timeframe = "15s"
	TF1m  Timeframe = "1m"
	TF5m  Timeframe = "5m"
	TF15m Timeframe = "15m"
	TF30m Timeframe = "30m"
	TF1h  Timeframe = "1h"
	TF1d  Timeframe = "1d"
)

// Bar is one OHLCV candle. Time is the bar OPEN time, unix seconds UTC - the
// representation the front-end lightweight-charts library consumes directly.
type Bar struct {
	Time   int64   `json:"t"`
	Open   float64 `json:"o"`
	High   float64 `json:"h"`
	Low    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume float64 `json:"v"`
}

// BarSource fetches OHLCV bars for a vendor symbol over [from, to].
// Implementations return bars sorted ascending by Time and drop null/incomplete
// candles rather than emitting zero-filled rows. An empty-but-valid range
// returns ErrNoData.
type BarSource interface {
	FetchBars(ctx context.Context, yahooSymbol string, tf Timeframe, from, to time.Time) ([]Bar, error)
}

// Sentinel errors let callers map a result to a UI state instead of a 5xx.
var (
	ErrNoData      = errors.New("marketdata: no bars for range")
	ErrRateLimited = errors.New("marketdata: source rate-limited")
	ErrUnsupported = errors.New("marketdata: symbol not mapped")
)

type tfMeta struct {
	yahooInterval string        // value for Yahoo &interval=
	maxLookback   time.Duration // how far back Yahoo serves this interval (conservative)
	dur           time.Duration // nominal bar width, for selection + aggregation math
}

// tfTable holds per-timeframe metadata. Lookbacks sit a touch under Yahoo's
// observed limits (1m ~30d, 5m/15m/30m ~60d, 60m ~730d) to avoid boundary gaps.
var tfTable = map[Timeframe]tfMeta{
	TF1s:  {"", 0, time.Second}, // exchange only, fetched on demand around short trades
	TF5s:  {"", 0, 5 * time.Second},
	TF15s: {"", 0, 15 * time.Second},
	TF1m:  {"1m", 25 * 24 * time.Hour, time.Minute},
	TF5m:  {"5m", 55 * 24 * time.Hour, 5 * time.Minute},
	TF15m: {"15m", 55 * 24 * time.Hour, 15 * time.Minute},
	TF30m: {"30m", 55 * 24 * time.Hour, 30 * time.Minute},
	TF1h:  {"60m", 700 * 24 * time.Hour, time.Hour},
	TF1d:  {"1d", 100 * 365 * 24 * time.Hour, 24 * time.Hour},
}

// tfLadder is finest -> coarsest. Used by selection (display tf) and coarsening.
// One-second bars sit outside it: a layer fetched on demand around short
// trades (FineBars), never a day's storage size.
var tfLadder = []Timeframe{TF1m, TF5m, TF15m, TF30m, TF1h, TF1d}

// Duration returns the nominal bar width.
func (tf Timeframe) Duration() time.Duration { return tfTable[tf].dur }

// Seconds returns the nominal bar width in seconds (for bucket math).
func (tf Timeframe) Seconds() int64 { return int64(tfTable[tf].dur / time.Second) }

// YahooInterval returns the Yahoo &interval= value for this timeframe.
func (tf Timeframe) YahooInterval() string { return tfTable[tf].yahooInterval }

func ladderIndex(tf Timeframe) int {
	for i, c := range tfLadder {
		if c == tf {
			return i
		}
	}
	return -1
}

// Coarser returns whichever of a, b is the coarser (longer) timeframe.
func Coarser(a, b Timeframe) Timeframe {
	if ladderIndex(b) > ladderIndex(a) {
		return b
	}
	return a
}

// TimeframesFrom returns tf and every coarser timeframe in the ladder, as
// strings - the bar sizes a chart can offer when its finest available data is
// tf (the client rolls the finest bars up to any of these).
func TimeframesFrom(tf Timeframe) []string {
	i := ladderIndex(tf)
	if i < 0 {
		return []string{string(tf)}
	}
	out := make([]string, 0, len(tfLadder)-i)
	for _, c := range tfLadder[i:] {
		out = append(out, string(c))
	}
	return out
}

// StorageTimeframe returns the finest timeframe Yahoo can still serve for a day
// of the given age (now - dayEnd). The prefetch/fetch path stores at this frame
// to capture maximum resolution before Yahoo ages it out; only 1m/5m/1h/1d are
// ever persisted (15m/30m are display-only roll-ups).
func StorageTimeframe(dayEnd, now time.Time) Timeframe {
	age := now.Sub(dayEnd)
	switch {
	case age <= tfTable[TF1m].maxLookback:
		return TF1m
	case age <= tfTable[TF5m].maxLookback:
		return TF5m
	case age <= tfTable[TF1h].maxLookback:
		return TF1h
	default:
		return TF1d
	}
}
