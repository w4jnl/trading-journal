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
	// AfterAccount, when set, runs once an account's days are fetched (the
	// excursion filler reads the fresh bars). Best-effort like the rest.
	AfterAccount func(ctx context.Context, accountID int64)

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
			p.processAccount(ctx, accountID, 0)
		}
	}
}

// ScheduleNightly runs a full refresh once a day at hh:mm local, until ctx is
// cancelled. It force-completes the last couple of days (a session only seen
// mid-day becomes a complete past day once it closes) and backfills any missing
// recent days. Mirrors backup.Schedule's daily-timer pattern.
func (p *Prefetcher) ScheduleNightly(ctx context.Context, hour, min int) {
	if p == nil {
		return
	}
	loc := p.Loc
	if loc == nil {
		loc = time.UTC
	}
	for {
		next := nextDaily(time.Now().In(loc), hour, min, loc)
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		p.logger().Info("marketdata: nightly refresh starting")
		p.RefreshAll(ctx, 2)
	}
}

// nextDaily returns the next hh:mm local instant strictly after now.
func nextDaily(now time.Time, hour, min int, loc *time.Location) time.Time {
	n := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
	if !n.After(now) {
		n = n.AddDate(0, 0, 1)
	}
	return n
}

// RefreshAll warms the cache for every active account's recent trade days,
// forcing a re-fetch of the last forceRecentDays days (to complete partials) and
// backfilling anything else still missing/upgradable. Best-effort; logs failures.
func (p *Prefetcher) RefreshAll(ctx context.Context, forceRecentDays int) {
	if p == nil {
		return
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT id FROM accounts WHERE active = 1`)
	if err != nil {
		p.logger().Warn("marketdata: nightly enumerate accounts", "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			p.logger().Warn("marketdata: nightly scan account", "err", err)
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		p.processAccount(ctx, id, forceRecentDays)
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
func (p *Prefetcher) processAccount(ctx context.Context, accountID int64, forceRecentDays int) {
	loc := p.Loc
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now()
	cutoff := now.AddDate(0, 0, -45)
	forceCutoff := startOfDay(now, loc).AddDate(0, 0, -forceRecentDays)

	rows, err := p.DB.QueryContext(ctx,
		`SELECT symbol_root, entry_time, exit_time
		   FROM trades
		  WHERE account_id = ?
		    AND (exit_time IS NULL OR exit_time >= ?)`,
		accountID, cutoff.UTC()) // stored as UTC text; a local instant would compare as if it were UTC
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
		// Force a re-fetch of the most recent days (the nightly catch-up) so a
		// session only seen mid-day is completed once it closes; otherwise skip
		// any day already cached and current.
		forced := forceRecentDays > 0 && !j.day.Before(forceCutoff)
		if !forced {
			if need, err := p.Cache.dayNeedsFetch(ctx, j.sym, j.day, now); err == nil && !need {
				continue
			}
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
	if p.AfterAccount != nil && ctx.Err() == nil {
		p.AfterAccount(ctx, accountID)
	}
}

// TradeDays returns the exchange-tz trading days a trade spans, padded by one day
// on each side (entry_day-1 .. exit_day+1) and including today but excluding the
// future (no data exists yet; today is warmed so its chart renders on first view,
// kept current by the cache's freshness TTL). A nil exit means the trade is still
// open, so the span runs to now. Shared by the prefetch worker and the admin
// coverage review so both agree on which days a trade needs.
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
		if !d.After(today) {
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
