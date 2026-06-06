package marketdata

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CachingSource fronts a BarSource with the price_bars / price_bar_days cache.
// It fetches whole exchange-tz trading days at the finest timeframe the day's
// age allows, marks completed days covered (so legitimate session gaps are not
// refetched forever), and serves windowed reads rolled up to a read timeframe.
//
// Loc is the exchange timezone used to bucket fills into trade days (CME =
// America/Chicago, which is also the app's default trading tz).
type CachingSource struct {
	DB     *sql.DB
	Source BarSource
	Loc    *time.Location
}

func (c *CachingSource) loc() *time.Location {
	if c.Loc != nil {
		return c.Loc
	}
	return time.UTC
}

func startOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func dayKey(day time.Time) string { return day.Format("2006-01-02") }

// daysInWindow returns the Loc-midnight days overlapping [from, to] inclusive.
func (c *CachingSource) daysInWindow(from, to time.Time) []time.Time {
	loc := c.loc()
	start := startOfDay(from, loc)
	end := startOfDay(to, loc)
	var out []time.Time
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	return out
}

// WindowCovered reports whether every PAST day the window overlaps has been
// fetched. A window that reaches into today or the future is never "covered" -
// an in-progress session must stay refetchable, and a freshly-made trade should
// show the load button rather than a stale partial.
func (c *CachingSource) WindowCovered(ctx context.Context, yahooSymbol string, from, to time.Time) (bool, error) {
	today := startOfDay(time.Now(), c.loc())
	for _, day := range c.daysInWindow(from, to) {
		if !day.Before(today) {
			return false, nil // today / future
		}
		present, err := c.dayPresent(ctx, yahooSymbol, day)
		if err != nil {
			return false, err
		}
		if !present {
			return false, nil
		}
	}
	return true, nil
}

func (c *CachingSource) dayPresent(ctx context.Context, yahooSymbol string, day time.Time) (bool, error) {
	var one int
	err := c.DB.QueryRowContext(ctx,
		`SELECT 1 FROM price_bar_days WHERE yahoo_symbol = ? AND day = ? LIMIT 1`,
		yahooSymbol, dayKey(day)).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// FetchDay fetches one exchange-tz trading day at the finest feasible timeframe
// and stores it. A past day is then marked covered; today/future days are left
// unmarked so they stay refetchable. ErrNoData (genuinely empty range) still
// marks a past day so it is not retried forever. The network call happens
// before any DB transaction so the single WAL connection is never held across
// the request.
func (c *CachingSource) FetchDay(ctx context.Context, yahooSymbol string, day time.Time) error {
	loc := c.loc()
	now := time.Now()
	today := startOfDay(now, loc)
	dayStart := startOfDay(day, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	tf := StorageTimeframe(dayEnd, now)

	bars, err := c.Source.FetchBars(ctx, yahooSymbol, tf, dayStart, dayEnd)
	if err != nil && err != ErrNoData {
		return err
	}
	if len(bars) > 0 {
		if err := c.storeBars(ctx, yahooSymbol, tf, bars); err != nil {
			return err
		}
	}
	if dayStart.Before(today) {
		if err := c.markDay(ctx, yahooSymbol, tf, dayKey(dayStart)); err != nil {
			return err
		}
	}
	return nil
}

// EnsureWindow fetches any day in the window not already cached. Past days that
// are already present are skipped; today is always (re)fetched so an in-progress
// session refreshes. Returns ErrRateLimited if the source throttles (so the
// caller can surface a transient state); other per-day errors are returned too.
func (c *CachingSource) EnsureWindow(ctx context.Context, yahooSymbol string, from, to time.Time) error {
	today := startOfDay(time.Now(), c.loc())
	for _, day := range c.daysInWindow(from, to) {
		if day.After(today) {
			continue // no data exists for the future
		}
		if day.Before(today) {
			present, err := c.dayPresent(ctx, yahooSymbol, day)
			if err != nil {
				return err
			}
			if present {
				continue
			}
		}
		if err := c.FetchDay(ctx, yahooSymbol, day); err != nil {
			return err
		}
	}
	return nil
}

// BarsForWindow returns the cached bars overlapping [from, to], rolled up to the
// coarsest stored timeframe present in that range (so a window spanning days
// stored at different frames yields one uniform series). covered reports
// WindowCovered for the same range.
func (c *CachingSource) BarsForWindow(ctx context.Context, yahooSymbol string, from, to time.Time) ([]Bar, Timeframe, bool, error) {
	covered, err := c.WindowCovered(ctx, yahooSymbol, from, to)
	if err != nil {
		return nil, "", false, err
	}
	rows, err := c.DB.QueryContext(ctx,
		`SELECT timeframe, bar_time, open, high, low, close, volume
		   FROM price_bars
		  WHERE yahoo_symbol = ? AND bar_time >= ? AND bar_time < ?
		  ORDER BY bar_time`,
		yahooSymbol, from.Unix(), to.Unix())
	if err != nil {
		return nil, "", covered, err
	}
	defer rows.Close()
	var bars []Bar
	readTf := Timeframe("")
	for rows.Next() {
		var tf string
		var b Bar
		if err := rows.Scan(&tf, &b.Time, &b.Open, &b.High, &b.Low, &b.Close, &b.Volume); err != nil {
			return nil, "", covered, err
		}
		bars = append(bars, b)
		readTf = Coarser(readTf, Timeframe(tf))
	}
	if err := rows.Err(); err != nil {
		return nil, "", covered, err
	}
	if len(bars) == 0 {
		return nil, "", covered, nil
	}
	// Days are disjoint in time and each is stored at a single timeframe, so
	// rolling the whole sorted set up to the coarsest present frame is safe.
	return AggregateTo(bars, readTf), readTf, covered, nil
}

func (c *CachingSource) storeBars(ctx context.Context, yahooSymbol string, tf Timeframe, bars []Bar) error {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("marketdata: begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO price_bars
		   (yahoo_symbol, timeframe, bar_time, open, high, low, close, volume, fetched_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("marketdata: prepare: %w", err)
	}
	defer stmt.Close()
	fetchedAt := time.Now().Unix()
	for _, b := range bars {
		if _, err := stmt.ExecContext(ctx, yahooSymbol, string(tf), b.Time,
			b.Open, b.High, b.Low, b.Close, b.Volume, fetchedAt); err != nil {
			return fmt.Errorf("marketdata: insert bar: %w", err)
		}
	}
	return tx.Commit()
}

func (c *CachingSource) markDay(ctx context.Context, yahooSymbol string, tf Timeframe, day string) error {
	_, err := c.DB.ExecContext(ctx,
		`INSERT OR REPLACE INTO price_bar_days (yahoo_symbol, timeframe, day, fetched_at)
		 VALUES (?, ?, ?, ?)`,
		yahooSymbol, string(tf), day, time.Now().Unix())
	return err
}
