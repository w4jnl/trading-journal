-- The day map (internal/levelmap): the chart feed and the built maps.
-- Comments here must stay ASCII-only (sqlc byte counting).

-- name: InsertLevelFeed :one
INSERT INTO level_feeds (user_id, market, trade_day, as_of, received_at, contract, payload)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: LatestLevelFeed :one
-- The newest post for the user, market and trading day.
SELECT * FROM level_feeds
WHERE user_id = ? AND market = ? AND trade_day = ?
ORDER BY as_of DESC, id DESC
LIMIT 1;

-- name: ListLevelFeedsNewest :many
-- The newest posts for the user, market and trading day, newest first:
-- the caller breaks a tie on as_of by the fuller post.
SELECT * FROM level_feeds
WHERE user_id = ? AND market = ? AND trade_day = ?
ORDER BY as_of DESC, id DESC
LIMIT ?;

-- name: ListLevelFeedDays :many
-- The trading days a market has posts for, newest first.
SELECT DISTINCT trade_day FROM level_feeds
WHERE user_id = ? AND market = ?
ORDER BY trade_day DESC
LIMIT ?;

-- name: ListLevelFeedMarkets :many
-- The markets with a post on the trading day.
SELECT DISTINCT market FROM level_feeds
WHERE user_id = ? AND trade_day = ?
ORDER BY market;

-- name: PurgeLevelFeeds :execrows
DELETE FROM level_feeds WHERE received_at < ?;

-- name: UpsertLevelMap :exec
INSERT INTO level_maps (user_id, market, trade_day, phase, as_of, data)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(user_id, market, trade_day, phase) DO UPDATE SET as_of = excluded.as_of, data = excluded.data;

-- name: GetLevelMap :one
SELECT * FROM level_maps
WHERE user_id = ? AND market = ? AND trade_day = ? AND phase = ?;

-- name: ListLevelMapDays :many
-- The trading days a market has a built map for, newest first.
SELECT DISTINCT trade_day FROM level_maps
WHERE user_id = ? AND market = ?
ORDER BY trade_day DESC
LIMIT ?;
