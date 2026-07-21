package topstepx

import (
	"time"

	"github.com/w4jnl/trading-journal/internal/markethours"
)

// InGlobexHours reports whether CME Globex is trading at t. Thin wrapper over
// markethours (the shared source of truth used by the ibkr scheduler too).
func InGlobexHours(t time.Time, chicago *time.Location) bool {
	return markethours.InGlobexHours(t, chicago)
}

// InRTHWindow reports whether t is in the Mon-Fri intraday fast-sync window
// [startMin, endMin) in Chicago wall-clock. Thin wrapper over markethours.
func InRTHWindow(t time.Time, chicago *time.Location, startMin, endMin int) bool {
	return markethours.InRTHWindow(t, chicago, startMin, endMin)
}
