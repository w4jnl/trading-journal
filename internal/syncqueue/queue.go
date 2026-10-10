// Package syncqueue is the per-user background job queue the broker syncs
// share (TopstepX, IBKR). One bounded channel is drained serially - the
// importer's FIFO rebuild wants a single writer - with single-flight per
// user, a per-user "ran recently" gap behind the throttled triggers, and the
// two-tier CME-hours scheduler that fans out to every user. Before this
// package each sync carried its own copy of all of it.
package syncqueue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/markethours"
)

// Options are the cadence and throttle knobs, straight from config.
type Options struct {
	Throttle     time.Duration // min gap between page-visit syncs per user; <=0 -> 15m
	APIThrottle  time.Duration // min gap between API-read-triggered syncs per user; <=0 disables that trigger
	SessionEvery time.Duration // scheduled cadence in the Globex window; 0 disables
	RTHEvery     time.Duration // faster cadence inside the RTH window; 0 disables
	RTHStartMin  int           // RTH window bounds, minutes-of-day Chicago
	RTHEndMin    int
	QueueSize    int // buffered jobs; <=0 -> 64
}

// Job is one unit of work: the user it concerns and a service-specific
// payload. An Untracked job skips the single-flight and throttle
// bookkeeping - TopstepX's per-account re-fetch is an explicit user action
// that must run even while a user-wide sync is pending, and must not reset
// the user's throttle clock when it finishes.
type Job[P any] struct {
	UserID    int64
	Payload   P
	Untracked bool
}

// Queue is the worker. A nil *Queue is a valid no-op receiver on every
// method, so a disabled sync needs no guards at its call sites.
type Queue[P any] struct {
	name string
	log  *slog.Logger
	opts Options
	loc  *time.Location // CME session clock for Schedule
	run  func(ctx context.Context, job Job[P]) error

	mu      sync.Mutex
	lastRun map[int64]time.Time
	pending map[int64]bool
	jobs    chan Job[P]
}

// schedulerTick is the resolution of the scheduler loop. Each tick is a
// couple of map lookups and wall-clock comparisons - firing (and the API
// traffic it implies) is governed by ScheduledInterval, not the tick rate.
const schedulerTick = time.Minute

// New wires a queue; run is the per-job work (serial, one at a time). name
// prefixes log lines ("topstepx", "ibkr").
func New[P any](name string, opts Options, logger *slog.Logger, run func(ctx context.Context, job Job[P]) error) *Queue[P] {
	if opts.Throttle <= 0 {
		opts.Throttle = 15 * time.Minute
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 64
	}
	if logger == nil {
		logger = slog.Default()
	}
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		logger.Warn(name+": America/Chicago tz unavailable, scheduler uses UTC", "err", err)
		loc = time.UTC
	}
	return &Queue[P]{
		name:    name,
		log:     logger,
		opts:    opts,
		loc:     loc,
		run:     run,
		lastRun: map[int64]time.Time{},
		pending: map[int64]bool{},
		jobs:    make(chan Job[P], opts.QueueSize),
	}
}

// MaybeEnqueueUser triggers a sync for the user unless one is already
// pending or ran within the page-visit throttle. Never blocks.
func (q *Queue[P]) MaybeEnqueueUser(userID int64) {
	if q == nil {
		return
	}
	q.EnqueueWithGap(userID, q.opts.Throttle)
}

// MaybeEnqueueUserAPI is the read-triggered twin for the JSON API and MCP
// tools: a consumer app reading trades keeps the user's broker data moving
// without anyone opening a page. Its own, tighter gap (APIThrottle) bounds
// the broker traffic a client polling every few seconds can cause; <=0
// disables the trigger. Never blocks.
func (q *Queue[P]) MaybeEnqueueUserAPI(userID int64) {
	if q == nil || q.opts.APIThrottle <= 0 {
		return
	}
	q.EnqueueWithGap(userID, q.opts.APIThrottle)
}

// EnqueueWithGap queues a tracked, whole-user job unless one is pending or
// the last one finished less than minGap ago. The scheduler passes half its
// cadence, so a sync the user just triggered by browsing is not repeated
// seconds later. Reports whether a job was queued.
func (q *Queue[P]) EnqueueWithGap(userID int64, minGap time.Duration) bool {
	if q == nil || userID == 0 {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending[userID] || time.Since(q.lastRun[userID]) < minGap {
		return false
	}
	select {
	case q.jobs <- Job[P]{UserID: userID}:
		q.pending[userID] = true
		return true
	default:
		q.log.Warn(q.name+": sync queue full, dropping", "user_id", userID)
		return false
	}
}

// EnqueueUntracked queues a job that bypasses the throttle and the
// single-flight bookkeeping (explicit user actions). Reports whether it fit.
func (q *Queue[P]) EnqueueUntracked(userID int64, payload P) bool {
	if q == nil || userID == 0 {
		return false
	}
	select {
	case q.jobs <- Job[P]{UserID: userID, Payload: payload, Untracked: true}:
		return true
	default:
		q.log.Warn(q.name+": sync queue full, dropping untracked job", "user_id", userID)
		return false
	}
}

// PendingForUser reports whether a tracked sync is queued or running for
// the user (drives the "syncing..." banner).
func (q *Queue[P]) PendingForUser(userID int64) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending[userID]
}

// WaitForUser blocks until no sync is pending for the user, the deadline
// passes, or ctx ends - the second half of a refresh read (nudge, wait, then
// read). Run clears pending only after the job's import has committed, so a
// read after this sees the fills the nudge pulled.
func (q *Queue[P]) WaitForUser(ctx context.Context, userID int64, deadline time.Time) {
	if q == nil {
		return
	}
	for q.PendingForUser(userID) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Run drains the queue until ctx is done. Jobs run serially: the importer's
// FIFO rebuild assumes one writer per (account, symbol) at a time.
func (q *Queue[P]) Run(ctx context.Context) {
	if q == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-q.jobs:
			if err := q.run(ctx, job); err != nil && !errors.Is(err, context.Canceled) {
				q.log.Error(q.name+" sync failed", "user_id", job.UserID, "err", err)
			}
			if !job.Untracked {
				q.MarkRan(job.UserID, time.Now())
			}
		}
	}
}

// Schedule runs the two-tier scheduled sync until ctx is done:
//
//   - RTH tier: every RTHEvery inside the RTH window (Mon-Fri,
//     RTHStartMin..RTHEndMin Chicago), catching intraday fills quickly while
//     the user is trading.
//   - Session tier: every SessionEvery through the rest of the CME Globex
//     session, keeping overnight activity reasonably fresh.
//   - Closed (Saturday, post-close Friday, pre-open Sunday, the daily
//     16:00-17:00 CT maintenance break unless the RTH window covers it): no
//     syncs.
//
// listUsers names who to fan out to on each firing (users with credentials,
// owners of enabled gateways). Cadence changes take effect at the window
// edges automatically because the interval is re-evaluated per tick.
func (q *Queue[P]) Schedule(ctx context.Context, listUsers func(ctx context.Context) ([]int64, error)) {
	if q == nil {
		return
	}
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	var lastFire time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		interval := q.ScheduledInterval(now)
		if interval <= 0 || now.Sub(lastFire) < interval {
			continue
		}
		lastFire = now
		users, err := listUsers(ctx)
		if err != nil {
			q.log.Error(q.name+": list sync users", "err", err)
			continue
		}
		q.log.Debug(q.name+": scheduled sync tick", "users", len(users), "cadence", interval.String())
		seen := map[int64]bool{}
		for _, uid := range users {
			if seen[uid] {
				continue
			}
			seen[uid] = true
			q.EnqueueWithGap(uid, interval/2)
		}
	}
}

// ScheduledInterval returns the cadence in effect at t: the RTH cadence
// inside the RTH window, the session cadence during Globex trading hours,
// 0 when the market is closed (or that tier is disabled in config).
func (q *Queue[P]) ScheduledInterval(t time.Time) time.Duration {
	if q == nil {
		return 0
	}
	if q.opts.RTHEvery > 0 && markethours.InRTHWindow(t, q.loc, q.opts.RTHStartMin, q.opts.RTHEndMin) {
		return q.opts.RTHEvery
	}
	if q.opts.SessionEvery > 0 && markethours.InGlobexHours(t, q.loc) {
		return q.opts.SessionEvery
	}
	return 0
}

// Len is the number of queued, not yet started jobs.
func (q *Queue[P]) Len() int {
	if q == nil {
		return 0
	}
	return len(q.jobs)
}

// Take removes the next queued job without running it (tests, and a
// service that wants to inspect before running).
func (q *Queue[P]) Take() (Job[P], bool) {
	if q == nil {
		var zero Job[P]
		return zero, false
	}
	select {
	case j := <-q.jobs:
		return j, true
	default:
		var zero Job[P]
		return zero, false
	}
}

// MarkPending flips the user's in-flight flag (tests simulate a running
// job with it).
func (q *Queue[P]) MarkPending(userID int64, pending bool) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending[userID] = pending
}

// MarkRan records a finished tracked job: clears pending and stamps the
// throttle clock. Run calls it; tests use it to place a user inside or
// outside a gap.
func (q *Queue[P]) MarkRan(userID int64, at time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending[userID] = false
	q.lastRun[userID] = at
}
