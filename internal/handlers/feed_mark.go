package handlers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/w4jnl/trading-journal/internal/levelmap"
	"github.com/w4jnl/trading-journal/internal/models"
)

// feedMarkMaxAge is how old a chart's mark may be and still count as a
// live price: the study posts once a minute while a chart runs, so three
// minutes of silence means the chart stopped.
const feedMarkMaxAge = 3 * time.Minute

// FeedMark is the last price a MotiveWave chart posted for the root's
// market today (docs/api-handoff-002-levels-feed.md): the newest post's
// mark and its asOf, from any user of the instance. ok is false without a
// post, or when the newest is older than feedMarkMaxAge.
func FeedMark(ctx context.Context, q *models.Queries, root string, now time.Time, chicago *time.Location) (float64, time.Time, bool) {
	market, ok := levelmap.MarketOf(root)
	if !ok || q == nil {
		return 0, time.Time{}, false
	}
	rows, err := q.ListLevelFeedsNewest(ctx, models.ListLevelFeedsNewestParams{Market: market, TradeDay: levelmap.TradingDay(now, chicago), Limit: 3})
	if err != nil {
		return 0, time.Time{}, false
	}
	for _, row := range rows {
		var f levelmap.Feed
		if json.Unmarshal([]byte(row.Payload), &f) != nil || f.Mark <= 0 {
			continue
		}
		if now.Sub(f.AsOf) > feedMarkMaxAge {
			return 0, time.Time{}, false
		}
		return f.Mark, f.AsOf, true
	}
	return 0, time.Time{}, false
}
