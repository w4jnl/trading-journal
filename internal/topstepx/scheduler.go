package topstepx

import (
	"context"
	"time"
)

// schedulerTick is the resolution of the scheduler loop. Each tick is a
// couple of map lookups and wall-clock comparisons - firing (and the API
// traffic it implies) is governed by scheduledInterval, not the tick rate.
const schedulerTick = time.Minute

// Schedule runs the two-tier scheduled sync until ctx is done:
//
//   - RTH tier: every Opts.RTHEvery inside the RTH window (Mon-Fri,
//     Opts.RTHStartMin..RTHEndMin Chicago - default equity RTH plus 1h each
//     side), catching intraday fills quickly while the user is trading.
//   - Session tier: every Opts.SessionEvery through the rest of the CME
//     Globex session (InGlobexHours, no extra margin), keeping overnight
//     activity reasonably fresh.
//   - Closed (Saturday, post-close Friday, pre-open Sunday, and the daily
//     16:00-17:00 CT maintenance break unless RTHWindow covers it): no syncs.
//
// Fan-out goes through enqueueWithGap with half the active cadence as the
// minimum gap, so a sync the user just triggered by browsing suppresses the
// scheduled one instead of doubling it. Cadence changes take effect at the
// window edges automatically because the interval is re-evaluated per tick.
func (s *SyncService) Schedule(ctx context.Context) {
	if s == nil {
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
		interval := s.scheduledInterval(now)
		if interval <= 0 || now.Sub(lastFire) < interval {
			continue
		}
		lastFire = now
		users, err := s.Queries.ListTopstepxSyncUsers(ctx)
		if err != nil {
			s.Logger.Error("topstepx: list sync users", "err", err)
			continue
		}
		s.Logger.Debug("topstepx: scheduled sync tick", "users", len(users), "cadence", interval.String())
		for _, uid := range users {
			s.enqueueWithGap(uid, interval/2)
		}
	}
}

// scheduledInterval returns the sync cadence in effect at t: the RTH cadence
// inside the RTH window, the session cadence during Globex trading hours,
// 0 when the market is closed (or that tier is disabled in config).
func (s *SyncService) scheduledInterval(t time.Time) time.Duration {
	if s.Opts.RTHEvery > 0 && InRTHWindow(t, s.chicago, s.Opts.RTHStartMin, s.Opts.RTHEndMin) {
		return s.Opts.RTHEvery
	}
	if s.Opts.SessionEvery > 0 && InGlobexHours(t, s.chicago) {
		return s.Opts.SessionEvery
	}
	return 0
}
