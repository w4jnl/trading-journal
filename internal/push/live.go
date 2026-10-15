package push

import "context"

// The Session Live Activity lives here in the next step; these hooks keep
// the triggers' shape.

// liveTick is the activity's look at a user on every tick.
func (s *Service) liveTick(ctx context.Context, uid int64, changed bool) {}

// tradeOnActivity reports whether a closed trade was carried as the
// Session activity's alert instead of a notification.
func (s *Service) tradeOnActivity(ctx context.Context, tg target, t closedTrade, badge int) bool {
	return false
}
