package topstepx

import "time"

// InGlobexHours reports whether CME Globex is trading at t. The session runs
// Sunday 17:00 to Friday 16:00 with a daily 16:00-17:00 maintenance break,
// all in Chicago wall-clock time — which is exactly how CME schedules it, so
// evaluating in America/Chicago handles DST for free. Exchange holidays are
// not modeled; a poll during one just comes back empty.
func InGlobexHours(t time.Time, chicago *time.Location) bool {
	lt := t.In(chicago)
	mins := lt.Hour()*60 + lt.Minute()
	const close16 = 16 * 60
	const open17 = 17 * 60
	switch lt.Weekday() {
	case time.Saturday:
		return false
	case time.Sunday:
		return mins >= open17
	case time.Friday:
		return mins < close16
	default: // Mon-Thu: closed only during the maintenance break
		return mins < close16 || mins >= open17
	}
}

// syncWindowMargin extends the scheduler's active window around the Globex
// session so late-settling fills after the close and pre-open activity are
// picked up without waiting for the next page visit.
const syncWindowMargin = 2 * time.Hour

// InSyncWindow reports whether the hourly scheduler should sync at t: within
// the Globex session or up to syncWindowMargin before it opens / after it
// closes. Composed as a time-shift of InGlobexHours — "the session was open
// within the last margin, is open now, or opens within the next margin" —
// so the two predicates can never drift apart. Effective window: Sunday
// 15:00 through Friday 18:00 Chicago time, Saturday off (the ±2h swallows
// the daily 16:00-17:00 maintenance break entirely).
func InSyncWindow(t time.Time, chicago *time.Location) bool {
	return InGlobexHours(t, chicago) ||
		InGlobexHours(t.Add(-syncWindowMargin), chicago) ||
		InGlobexHours(t.Add(syncWindowMargin), chicago)
}

// InRTHWindow reports whether t falls in the fast-sync window: Monday-Friday
// between startMin and endMin (minutes-of-day, Chicago wall clock, end
// exclusive). The configured default covers equity-futures RTH 08:30-15:00
// CT plus one hour before and after (07:30-16:00).
func InRTHWindow(t time.Time, chicago *time.Location, startMin, endMin int) bool {
	lt := t.In(chicago)
	switch lt.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	mins := lt.Hour()*60 + lt.Minute()
	return mins >= startMin && mins < endMin
}
