package marketdata

import (
	"context"
	"time"
)

// LatestQuote is the most recent price this cache knows for a vendor symbol:
// the close of the newest cached bar and that bar's time, so callers can show
// how fresh it is.
type LatestQuote struct {
	Price float64
	At    time.Time
}

// latestLookbackDays is how far back LatestPrice looks for a bar, so a
// weekend or holiday still yields the last session's close.
const latestLookbackDays = 5

// LatestPrice returns the newest cached close for yahooSymbol. It first lets
// EnsureWindow top up the recent days - today is re-fetched only when the
// intraday TTL has lapsed, so a dashboard that renders often does not hammer
// the vendor - then reads whatever is cached. A fetch failure is not fatal:
// the cached bars are still used, and only an empty cache yields ErrNoData.
// The price is the front-month continuous quote for the root (see
// YahooSymbol), which is what an open position in the current contract wants.
func (c *CachingSource) LatestPrice(ctx context.Context, yahooSymbol string) (LatestQuote, error) {
	now := time.Now()
	from := now.AddDate(0, 0, -latestLookbackDays)
	fetchErr := c.EnsureWindow(ctx, yahooSymbol, from, now)
	bars, _, _, err := c.BarsForWindow(ctx, yahooSymbol, from, now)
	if err != nil {
		return LatestQuote{}, err
	}
	if len(bars) == 0 {
		if fetchErr != nil {
			return LatestQuote{}, fetchErr
		}
		return LatestQuote{}, ErrNoData
	}
	last := bars[len(bars)-1]
	return LatestQuote{Price: last.Close, At: time.Unix(last.Time, 0).UTC()}, nil
}
