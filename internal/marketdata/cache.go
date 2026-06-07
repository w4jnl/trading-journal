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
	now := time.Now()
	today := startOfDay(now, c.loc())
	for _, day := range c.daysInWindow(from, to) {
		if !day.Before(today) {
			return false, nil // today / future
		}
		// "covered" means present AND at the finest timeframe now available -
		// a day cached only at a coarser size is treated as not covered so it
		// gets upgraded (e.g. a 5m day that is now within Yahoo's 1m window).
		need, err := c.dayNeedsFetch(ctx, yahooSymbol, day, now)
		if err != nil {
			return false, err
		}
		if need {
			return false, nil
		}
	}
	return true, nil
}

// CoveredDays returns the set of exchange-tz day keys (YYYY-MM-DD) already
// cached for a symbol on or after `since` (any timeframe). Used by the admin
// coverage review to tell which trade days still have gaps.
func (c *CachingSource) CoveredDays(ctx context.Context, yahooSymbol string, since time.Time) (map[string]bool, error) {
	rows, err := c.DB.QueryContext(ctx,
		`SELECT DISTINCT day FROM price_bar_days WHERE yahoo_symbol = ? AND day >= ?`,
		yahooSymbol, dayKey(startOfDay(since, c.loc())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, rows.Err()
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
	return c.replaceDay(ctx, yahooSymbol, tf, dayStart, dayEnd, bars, dayStart.Before(today))
}

// EnsureWindow fetches any day in the window not already cached. Past days that
// are already present are skipped; today is always (re)fetched so an in-progress
// session refreshes. Returns ErrRateLimited if the source throttles (so the
// caller can surface a transient state); other per-day errors are returned too.
func (c *CachingSource) EnsureWindow(ctx context.Context, yahooSymbol string, from, to time.Time) error {
	now := time.Now()
	today := startOfDay(now, c.loc())
	for _, day := range c.daysInWindow(from, to) {
		if day.After(today) {
			continue // no data exists for the future
		}
		if day.Before(today) {
			need, err := c.dayNeedsFetch(ctx, yahooSymbol, day, now)
			if err != nil {
				return err
			}
			if !need {
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

// replaceDay atomically swaps a day's cached data: it clears any existing bars
// in the day's time range plus the day's coverage rows, inserts the freshly
// fetched bars at tf, and (for a completed past day) records coverage at tf.
// The delete-then-insert makes a re-fetch at a finer timeframe an in-place
// upgrade rather than leaving stale coarser bars behind. An empty fetch (a
// genuine no-data day) still records coverage so it isn't retried forever,
// without disturbing any bars already present. Network already happened before
// this call, so the single WAL connection is only held for the quick write.
func (c *CachingSource) replaceDay(ctx context.Context, yahooSymbol string, tf Timeframe, dayStart, dayEnd time.Time, bars []Bar, past bool) error {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("marketdata: begin: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()

	if len(bars) > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM price_bars WHERE yahoo_symbol = ? AND bar_time >= ? AND bar_time < ?`,
			yahooSymbol, dayStart.Unix(), dayEnd.Unix()); err != nil {
			return fmt.Errorf("marketdata: clear bars: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM price_bar_days WHERE yahoo_symbol = ? AND day = ?`,
			yahooSymbol, dayKey(dayStart)); err != nil {
			return fmt.Errorf("marketdata: clear day: %w", err)
		}
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR REPLACE INTO price_bars
			   (yahoo_symbol, timeframe, bar_time, open, high, low, close, volume, fetched_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("marketdata: prepare: %w", err)
		}
		defer stmt.Close()
		for _, b := range bars {
			if _, err := stmt.ExecContext(ctx, yahooSymbol, string(tf), b.Time,
				b.Open, b.High, b.Low, b.Close, b.Volume, now); err != nil {
				return fmt.Errorf("marketdata: insert bar: %w", err)
			}
		}
	}
	if past {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO price_bar_days (yahoo_symbol, timeframe, day, fetched_at)
			 VALUES (?, ?, ?, ?)`,
			yahooSymbol, string(tf), dayKey(dayStart), now); err != nil {
			return fmt.Errorf("marketdata: mark day: %w", err)
		}
	}
	return tx.Commit()
}

// dayNeedsFetch reports whether a day should be (re)fetched: true when it has no
// cached coverage at all, or when its finest cached timeframe is coarser than
// the finest now available (StorageTimeframe) - i.e. a resolution upgrade is
// possible, e.g. a day stored at 5m that has since come within Yahoo's 1m
// window. A day already at the finest available size returns false.
func (c *CachingSource) dayNeedsFetch(ctx context.Context, yahooSymbol string, day, now time.Time) (bool, error) {
	rows, err := c.DB.QueryContext(ctx,
		`SELECT timeframe FROM price_bar_days WHERE yahoo_symbol = ? AND day = ?`,
		yahooSymbol, dayKey(day))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	finest := -1
	for rows.Next() {
		var tf string
		if err := rows.Scan(&tf); err != nil {
			return false, err
		}
		if idx := ladderIndex(Timeframe(tf)); idx >= 0 && (finest == -1 || idx < finest) {
			finest = idx
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if finest == -1 {
		return true, nil // not cached at all
	}
	optimal := ladderIndex(StorageTimeframe(day.AddDate(0, 0, 1), now))
	return finest > optimal, nil // stored coarser than now-available -> upgrade
}
