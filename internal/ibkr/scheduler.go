package ibkr

import (
	"context"
	"time"

	"github.com/w4jnl/trading-journal/internal/markethours"
)

// schedulerTick is the resolution of the scheduler loop; firing (and the API
// traffic it implies) is governed by scheduledInterval, not the tick rate.
const schedulerTick = time.Minute

// Schedule runs the two-tier scheduled poll until ctx is done, mirroring
// TopstepX: the RTH cadence inside the RTH window, the session cadence during
// CME Globex hours, and nothing when the market is closed. Each enabled
// gateway's owner is enqueued once per tick through enqueueWithGap (half the
// active cadence as the min gap), so a sync the user just triggered by
// browsing suppresses the scheduled one instead of doubling it. Unlike the
// old flat poller, this stays quiet overnight/weekends.
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
		gws, err := s.Queries.ListEnabledIbkrGateways(ctx)
		if err != nil {
			s.Logger.Error("ibkr: list gateways", "err", err)
			continue
		}
		seen := map[int64]bool{}
		for _, gw := range gws {
			if seen[gw.UserID] {
				continue
			}
			seen[gw.UserID] = true
			s.enqueueWithGap(gw.UserID, interval/2)
		}
	}
}

// scheduledInterval returns the cadence in effect at t: the RTH cadence inside
// the RTH window, the session cadence during Globex trading hours, 0 when the
// market is closed (or that tier is disabled in config).
func (s *SyncService) scheduledInterval(t time.Time) time.Duration {
	if s.Opts.RTHEvery > 0 && markethours.InRTHWindow(t, s.chicago, s.Opts.RTHStartMin, s.Opts.RTHEndMin) {
		return s.Opts.RTHEvery
	}
	if s.Opts.SessionEvery > 0 && markethours.InGlobexHours(t, s.chicago) {
		return s.Opts.SessionEvery
	}
	return 0
}
