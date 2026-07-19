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
