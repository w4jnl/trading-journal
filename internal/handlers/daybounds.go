package handlers

import "time"

// Day and range bounds for SQL.
//
// Times are stored as UTC text (modernc/sqlite writes time.Time with
// t.String(): "2026-10-09 22:30:00 +0000 UTC") and query parameters bind the
// same way, so "exit_time >= ?" is a TEXT comparison. A bound built at local
// midnight ("2026-10-10 00:00:00 +0200 CEST") therefore sorts as if it were
// 00:00 UTC - the offset is silently ignored - and every fill closed between
// 00:00 and 02:00 Amsterdam lands on the previous day. The /trades page
// learned this first (buildWhere); every other range filter must go through
// these helpers, which return UTC instants. daily_stats is unaffected: it is
// bucketed in Go.

// dayBoundsUTC returns the half-open [from, to) UTC window for the trading
// timezone calendar day that starts at dayLocal (midnight in that zone).
func dayBoundsUTC(dayLocal time.Time) (from, to time.Time) {
	return dayLocal.UTC(), dayLocal.AddDate(0, 0, 1).UTC()
}

// daysBoundsUTC returns the UTC window covering n calendar days starting at
// startLocal (midnight in the trading timezone).
func daysBoundsUTC(startLocal time.Time, n int) (from, to time.Time) {
	return startLocal.UTC(), startLocal.AddDate(0, 0, n).UTC()
}

// parseLocalDay parses a YYYY-MM-DD filter value as midnight in tz.
func parseLocalDay(s string, tz *time.Location) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", s, tz)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
