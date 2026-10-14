package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Bar-cache retention. price_bars is the largest table in the database and
// grows without bound - one-minute bars for a handful of roots add about
// 2.5 million rows a year - while the trade-replay charts only ever look
// back a few months (Yahoo serves intraday bars for ~60 days anyway). Daily
// bars are small and kept; intraday bars older than the window are pruned
// once a day, together with the coverage rows that would otherwise claim
// those days are cached.

// PurgeIntraday deletes intraday bars (every timeframe but 1d) whose bar
// time is before cutoff, and the matching day-coverage rows. Returns the
// number of bar rows removed.
func (c *CachingSource) PurgeIntraday(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("marketdata: begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM price_bars WHERE timeframe <> '1d' AND bar_time < ?`, cutoff.Unix())
	if err != nil {
		return 0, fmt.Errorf("marketdata: purge bars: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM price_bar_days WHERE timeframe <> '1d' AND day < ?`, cutoff.In(c.loc()).Format("2006-01-02")); err != nil {
		return 0, fmt.Errorf("marketdata: purge days: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM price_bar_windows WHERE to_time < ?`, cutoff.Unix()); err != nil {
		return 0, fmt.Errorf("marketdata: purge windows: %w", err)
	}
	return n, tx.Commit()
}

// Retention runs PurgeIntraday shortly after boot and then once a day.
type Retention struct {
	Cache  *CachingSource
	Days   int // 0 disables
	Logger *slog.Logger
}

// Start blocks until ctx is done; run it in its own goroutine.
func (r Retention) Start(ctx context.Context) {
	if r.Days <= 0 || r.Cache == nil {
		return
	}
	log := r.Logger
	if log == nil {
		log = slog.Default()
	}
	run := func() {
		cutoff := time.Now().AddDate(0, 0, -r.Days)
		n, err := r.Cache.PurgeIntraday(ctx, cutoff)
		if err != nil {
			log.Warn("bar-cache retention failed", "err", err)
			return
		}
		if n > 0 {
			log.Info("bar-cache retention", "removed_bars", n, "older_than", cutoff.Format("2006-01-02"))
		}
	}
	// A short delay keeps boot fast and lets the first page loads win the
	// single SQLite writer.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	run()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
