package journal

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseISOWeek parses a "YYYY-Www" week id into the Monday that starts it
// (midnight in tz) plus the ISO year and week it resolves to.
func ParseISOWeek(s string, tz *time.Location) (monday time.Time, isoYear, isoWeek int, err error) {
	parts := strings.SplitN(s, "-W", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, 0, fmt.Errorf("invalid week id %q", s)
	}
	year, err1 := strconv.Atoi(parts[0])
	week, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || week < 1 || week > 53 {
		return time.Time{}, 0, 0, fmt.Errorf("invalid week id %q", s)
	}
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, tz)
	wd := int(jan4.Weekday())
	if wd == 0 {
		wd = 7
	}
	week1Monday := jan4.AddDate(0, 0, 1-wd)
	monday = week1Monday.AddDate(0, 0, (week-1)*7)
	y, w := monday.ISOWeek()
	return monday, y, w, nil
}
