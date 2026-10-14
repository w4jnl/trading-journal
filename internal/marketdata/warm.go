package marketdata

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/w4jnl/trading-journal/internal/markethours"
)

// ---- Session warm-up ---------------------------------------------------
//
// The cache fills on demand and serves today from a copy up to the intraday
// TTL old. For the instruments being traded that is too stale for what
// reads the session live - the open position's sheet, the guardrail, the
// AI levels - so the Warmer keeps today and the previous two trading days
// complete at one minute for every root with a fill in the last days,
// re-fetching today on its own cadence while Globex trades. One request
// per root per tick, against the same cache the charts read.

// WarmDays is how many past days the warm-up keeps complete, besides today.
const WarmDays = 2

// Warmer keeps the traded roots' recent sessions fresh.
type Warmer struct {
	Cache   *CachingSource
	DB      *sql.DB        // which roots are traded (executions of the last days)
	Chicago *time.Location // CME clock for the trading-hours gate
	Every   time.Duration  // the cadence while Globex trades; 0 disables
	Logger  *slog.Logger
	// RecentDays bounds how far back a fill still makes its root warm (default 5).
	RecentDays int
	// Roots overrides the query when set (tests, or a fixed list).
	Roots func(ctx context.Context) ([]string, error)
}

func (w *Warmer) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// Start runs the warm-up until ctx ends: once at boot, then every Every
// while Globex trades (nothing changes outside the session).
func (w *Warmer) Start(ctx context.Context) {
	if w == nil || w.Cache == nil || w.Every <= 0 {
		return
	}
	w.Tick(ctx)
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if markethours.InGlobexHours(time.Now(), w.chicago()) {
				w.Tick(ctx)
			}
		}
	}
}

func (w *Warmer) chicago() *time.Location {
	if w.Chicago != nil {
		return w.Chicago
	}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		return time.UTC
	}
	return loc
}

// Tick warms every traded root once: today is re-fetched, the previous
// WarmDays are completed if missing. Errors are logged; a rate limit stops
// the round so the next tick starts calm.
func (w *Warmer) Tick(ctx context.Context) {
	syms, err := w.symbols(ctx)
	if err != nil {
		w.logger().Warn("session warm-up: roots", "err", err)
		return
	}
	now := time.Now()
	for _, sym := range syms {
		if err := w.Cache.EnsureWindow(ctx, sym, now.AddDate(0, 0, -WarmDays), now.Add(-24*time.Hour)); err != nil {
			w.logger().Warn("session warm-up: past days", "symbol", sym, "err", err)
			if err == ErrRateLimited {
				return
			}
		}
		if err := w.Cache.RefreshToday(ctx, sym); err != nil {
			w.logger().Warn("session warm-up: today", "symbol", sym, "err", err)
			if err == ErrRateLimited {
				return
			}
		}
	}
}

// symbols lists the vendor symbols of the roots with a fill in the last
// RecentDays (every user: the cache is shared), mapped through YahooSymbol.
func (w *Warmer) symbols(ctx context.Context) ([]string, error) {
	if w.Roots != nil {
		roots, err := w.Roots(ctx)
		if err != nil {
			return nil, err
		}
		return mapRoots(roots), nil
	}
	days := w.RecentDays
	if days <= 0 {
		days = 5
	}
	rows, err := w.DB.QueryContext(ctx,
		`SELECT DISTINCT symbol_root FROM executions WHERE deleted_at IS NULL AND exec_time >= ? ORDER BY symbol_root`,
		time.Now().AddDate(0, 0, -days).UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		roots = append(roots, r)
	}
	return mapRoots(roots), rows.Err()
}

func mapRoots(roots []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range roots {
		if sym, ok := YahooSymbol(r); ok && !seen[sym] {
			seen[sym] = true
			out = append(out, sym)
		}
	}
	return out
}

// RefreshToday re-fetches the current trading day whatever the TTL says:
// the warm-up's own cadence decides freshness for the traded roots.
func (c *CachingSource) RefreshToday(ctx context.Context, yahooSymbol string) error {
	return c.FetchDay(ctx, yahooSymbol, startOfDay(time.Now(), c.loc()))
}

// CachedLatest is the newest cached bar of the last two days, without a
// fetch: what a worker that must never wait on a vendor reads. ok is false
// when nothing is cached.
func (c *CachingSource) CachedLatest(ctx context.Context, yahooSymbol string) (Bar, bool) {
	row := c.DB.QueryRowContext(ctx,
		`SELECT bar_time, open, high, low, close, volume FROM price_bars
		 WHERE yahoo_symbol = ? AND bar_time >= ? ORDER BY bar_time DESC LIMIT 1`,
		yahooSymbol, time.Now().Add(-48*time.Hour).Unix())
	var b Bar
	if err := row.Scan(&b.Time, &b.Open, &b.High, &b.Low, &b.Close, &b.Volume); err != nil {
		return Bar{}, false
	}
	return b, true
}

// RecentBars returns the cached bars of the last days at the finest stored
// size (one minute for the warmed roots), ascending, without a fetch.
func (c *CachingSource) RecentBars(ctx context.Context, yahooSymbol string, days int, asOf time.Time) ([]Bar, Timeframe, error) {
	from := asOf.AddDate(0, 0, -days)
	bars, tf, _, err := c.BarsForWindow(ctx, yahooSymbol, from, asOf)
	return bars, tf, err
}
