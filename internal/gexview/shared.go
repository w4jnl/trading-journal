// Shared helpers the view builders pulled along (ET clock, wall lookup).
package gexview

import (
	"github.com/w4jnl/trading-journal/internal/gex"
	"sync"
	"time"
)

// StaleAfter is how far as_of may lag the trade moment before the context
// is labeled stale. Snapshots land on a 5-minute grid during 09:50-16:00 ET,
// so anything beyond ~15m means the trade sat outside the options session and
// got the prior close's levels (correct at-or-before behavior, but the block
// must be honest about it).
const StaleAfter = 15 * time.Minute

// Eastern lazily loads the options-session timezone. The gamma grid is an
// ET concept, so the block's time labels are ET regardless of the journal's
// trading timezone.
var (
	ETOnce sync.Once
	ETLoc  *time.Location
)

func Eastern() *time.Location {
	ETOnce.Do(func() {
		if l, err := time.LoadLocation("America/New_York"); err == nil {
			ETLoc = l
		} else {
			ETLoc = time.UTC
		}
	})
	return ETLoc
}

// Wall picks the snapshot's all-book wall of the given type. Since engine
// v0.5.0 the wall types also appear with bucket "0dte" (today's expiring
// book), so matching on type alone would misread a 0DTE wall as the all-book
// level; the bucket guard is contract-mandated (engine design doc, update
// 002). Nil when the snapshot has no such level.
func Wall(s *gex.Snapshot, typ string) *gex.Level {
	return LevelIn(s, typ, "all")
}

// LevelIn picks the snapshot's level of the given type and bucket; nil
// when absent.
func LevelIn(s *gex.Snapshot, typ, bucket string) *gex.Level {
	for i := range s.Levels {
		l := &s.Levels[i]
		if l.Type == typ && l.Bucket == bucket {
			return l
		}
	}
	return nil
}
