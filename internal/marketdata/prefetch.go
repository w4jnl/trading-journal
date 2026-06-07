package marketdata

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// Prefetcher warms the bar cache in the background after an import, so charts
// for freshly-imported trades render instantly on first view (and fine
// intraday bars are captured before Yahoo ages them out). It is best-effort:
// failures are logged, never surfaced to the import path.
type Prefetcher struct {
	Cache  *CachingSource
	DB     *sql.DB        // enumerate an account's recent trades
	Loc    *time.Location // exchange tz for day bucketing
	Logger *slog.Logger

	jobs  chan int64
	delay time.Duration // throttle between Yahoo day-fetches
}

// NewPrefetcher builds a Prefetcher with a buffered queue and a polite throttle.
func NewPrefetcher(cache *CachingSource, db *sql.DB, loc *time.Location) *Prefetcher {
	return &Prefetcher{
		Cache: cache,
		DB:    db,
		Loc:   loc,
		jobs:  make(chan int64, 256),
		delay: 1500 * time.Millisecond,
	}
}

func (p *Prefetcher) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// EnqueueAccount queues an account for background prefetch. Non-blocking: a full
// queue drops the request (charts then fall back to the on-view load button).
// Satisfies the importer's BarPrefetcher interface.
func (p *Prefetcher) EnqueueAccount(_ context.Context, accountID int64) {
	if p == nil {
		return
	}
	select {
	case p.jobs <- accountID:
	default:
		p.logger().Warn("marketdata: prefetch queue full, dropping", "account_id", accountID)
	}
}

// Run is the worker loop; it returns when ctx is cancelled.
func (p *Prefetcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case accountID := <-p.jobs:
			p.processAccount(ctx, accountID)
		}
	}
}

type prefetchJob struct {
	sym string
	day time.Time
}

// processAccount enumerates the account's recent trades, expands each to its
// [entry_day-1 .. exit_day+1] span per Yahoo symbol, dedupes, and fetches every
// uncovered completed day with a polite throttle. Days older than ~45 days are
// ignored (Yahoo no longer serves fine intraday for them, so eager fetch adds
// little) and today/future are left to the lazy on-view path.
func (p *Prefetcher) processAccount(ctx context.Context, accountID int64) {
	loc := p.Loc
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now()
	cutoff := now.AddDate(0, 0, -45)

	rows, err := p.DB.QueryContext(ctx,
		`SELECT symbol_root, entry_time, exit_time
		   FROM trades
		  WHERE account_id = ?
		    AND (exit_time IS NULL OR exit_time >= ?)`,
		accountID, cutoff)
	if err != nil {
		p.logger().Warn("marketdata: prefetch enumerate trades", "account_id", accountID, "err", err)
		return
	}

	seen := map[string]bool{}
	var jobs []prefetchJob
	for rows.Next() {
		var root string
		var entry time.Time
		var exit sql.NullTime
		if err := rows.Scan(&root, &entry, &exit); err != nil {
			p.logger().Warn("marketdata: prefetch scan trade", "err", err)
			continue
		}
		sym, ok := YahooSymbol(root)
		if !ok {
			continue
		}
		var exitPtr *time.Time
		if exit.Valid {
			e := exit.Time
			exitPtr = &e
		}
		for _, d := range TradeDays(entry, exitPtr, loc, now) {
			key := sym + "|" + dayKey(d)
			if seen[key] {
				continue
			}
			seen[key] = true
			jobs = append(jobs, prefetchJob{sym: sym, day: d})
		}
	}
	rows.Close()

	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if need, err := p.Cache.dayNeedsFetch(ctx, j.sym, j.day, now); err == nil && !need {
			continue
		}
		if err := p.Cache.FetchDay(ctx, j.sym, j.day); err != nil {
			if err == ErrRateLimited {
				p.logger().Warn("marketdata: prefetch rate-limited, backing off", "symbol", j.sym)
				if !sleepCtx(ctx, 30*time.Second) {
					return
				}
				continue
			}
			p.logger().Warn("marketdata: prefetch day", "symbol", j.sym, "day", dayKey(j.day), "err", err)
		}
		if !sleepCtx(ctx, p.delay) {
			return
		}
	}
}

// TradeDays returns the completed exchange-tz trading days a trade spans, padded
// by one day on each side (entry_day-1 .. exit_day+1) and excluding today and
// the future (an in-progress session is fetched lazily, not prefetched). A nil
// exit means the trade is still open, so the span runs to now. Shared by the
// prefetch worker and the admin coverage review so both agree on which days a
// trade needs.
func TradeDays(entry time.Time, exit *time.Time, loc *time.Location, now time.Time) []time.Time {
	if loc == nil {
		loc = time.UTC
	}
	end := now
	if exit != nil {
		end = *exit
	}
	today := startOfDay(now, loc)
	d0 := startOfDay(entry, loc).AddDate(0, 0, -1)
	d1 := startOfDay(end, loc).AddDate(0, 0, 1)
	var out []time.Time
	for d := d0; !d.After(d1); d = d.AddDate(0, 0, 1) {
		if d.Before(today) {
			out = append(out, d)
		}
	}
	return out
}

// sleepCtx sleeps for d or until ctx is done. Returns false if ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
