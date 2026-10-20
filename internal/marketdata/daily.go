package marketdata

import (
	"context"
	"sync"
	"time"
)

// ---- Daily history -------------------------------------------------------
//
// The day map (internal/levelmap) reads a few months of daily bars per
// market. The per-day cache fetches a day at a time, and older days are
// stored at an hour, so filling a window that long through it would mean
// hundreds of vendor calls on a first view. The daily series is one call
// instead, held in memory for an hour; when the vendor answers nothing
// the cached bars rolled up to days stand in.

const dailyTTL = time.Hour

type dailyEntry struct {
	bars      []Bar
	from, to  time.Time
	fetchedAt time.Time
}

// DailyBars returns the vendor's daily bars for [from, to], ascending,
// from a one-hour memory cache.
func (c *CachingSource) DailyBars(ctx context.Context, yahooSymbol string, from, to time.Time) ([]Bar, error) {
	c.dailyMu.Lock()
	if c.daily == nil {
		c.daily = map[string]dailyEntry{}
	}
	e, ok := c.daily[yahooSymbol]
	c.dailyMu.Unlock()
	now := time.Now()
	if ok && now.Sub(e.fetchedAt) < dailyTTL && !from.Before(e.from) && !to.After(e.to) {
		return sliceBars(e.bars, from, to), nil
	}
	var bars []Bar
	var err error
	if c.Source != nil {
		bars, err = c.Source.FetchBars(ctx, yahooSymbol, TF1d, from, to)
		if err != nil && err != ErrNoData {
			c.logger().Debug("marketdata: daily series unavailable, rolling up the cache", "symbol", yahooSymbol, "err", err)
		}
	}
	if len(bars) == 0 {
		cached, _, _, cerr := c.BarsForWindow(ctx, yahooSymbol, from, to)
		if cerr != nil {
			return nil, cerr
		}
		bars = AggregateTo(cached, TF1d)
		if err == nil && len(bars) == 0 {
			err = ErrNoData
		}
		if len(bars) == 0 {
			return nil, err
		}
		return bars, nil
	}
	c.dailyMu.Lock()
	c.daily[yahooSymbol] = dailyEntry{bars: bars, from: from, to: to, fetchedAt: now}
	c.dailyMu.Unlock()
	return sliceBars(bars, from, to), nil
}

func sliceBars(bars []Bar, from, to time.Time) []Bar {
	var out []Bar
	for _, b := range bars {
		t := time.Unix(b.Time, 0)
		if !t.Before(from) && !t.After(to) {
			out = append(out, b)
		}
	}
	return out
}

// dailyCache is the memory the method above keeps on the source.
type dailyCache struct {
	dailyMu sync.Mutex
	daily   map[string]dailyEntry
}
