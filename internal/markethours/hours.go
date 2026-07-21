// Package markethours holds the CME session/RTH predicates shared by the
// broker sync schedulers (topstepx, ibkr) so their market-hours gating can
// never drift apart.
package markethours

import "time"

// InGlobexHours reports whether CME Globex is trading at t. The session runs
// Sunday 17:00 to Friday 16:00 with a daily 16:00-17:00 maintenance break,
// all in Chicago wall-clock time - which is exactly how CME schedules it, so
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
