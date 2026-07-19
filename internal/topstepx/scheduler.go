package topstepx

import (
	"context"
	"time"
)

// Schedule runs an hourly sync fan-out at five past each hour, but only while
// CME Globex is trading (see InGlobexHours) — outside those hours nothing
// trades, so nothing new can appear. Each eligible user goes through
// MaybeEnqueueUser, so the page-visit throttle still dedups against a sync a
// user just triggered by browsing. Mirrors the backup.Schedule timer pattern.
func (s *SyncService) Schedule(ctx context.Context) {
	if s == nil {
		return
	}
	for {
		next := nextHourlyTick(time.Now())
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if !InGlobexHours(time.Now(), s.chicago) {
			s.Logger.Debug("topstepx: scheduler tick outside CME hours, skipped")
			continue
		}
		users, err := s.Queries.ListTopstepxSyncUsers(ctx)
		if err != nil {
			s.Logger.Error("topstepx: list sync users", "err", err)
			continue
		}
		s.Logger.Debug("topstepx: hourly sync tick", "users", len(users))
		for _, uid := range users {
			s.MaybeEnqueueUser(uid)
		}
	}
}

// nextHourlyTick returns the next hh:05:00 after now.
func nextHourlyTick(now time.Time) time.Time {
	tick := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 5, 0, 0, now.Location())
	if !tick.After(now) {
		tick = tick.Add(time.Hour)
	}
	return tick
}
