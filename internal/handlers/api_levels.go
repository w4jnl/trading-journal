package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/levelmap"
	"github.com/w4jnl/trading-journal/internal/models"
)

// APILevelsPost: POST /api/v1/levels/{market} - the session map a chart
// posts (docs/api-handoff-002-levels-feed.md). Every accepted post is
// kept; the newest asOf per market and trading day is the map's overlay.
func (h *Handlers) APILevelsPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	market := r.PathValue("market")
	if !levelmap.IsMarket(market) {
		writeJSONError(w, http.StatusBadRequest, "unknown market: one of ES, NQ, YM, RTY, CL, GC")
		return
	}
	var f levelmap.Feed
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&f); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body over 16 KB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	now := time.Now()
	if err := f.Validate(now); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	uid := userID(ctx)
	day := levelmap.TradingDay(f.AsOf, h.chicago())
	newest := true
	if cur, err := h.Queries.LatestLevelFeed(ctx, models.LatestLevelFeedParams{UserID: uid, Market: market, TradeDay: day}); err == nil && cur.AsOf.After(f.AsOf) {
		newest = false
	}
	payload, _ := json.Marshal(f)
	if _, err := h.Queries.InsertLevelFeed(ctx, models.InsertLevelFeedParams{UserID: uid, Market: market, TradeDay: day, AsOf: f.AsOf.UTC(), ReceivedAt: now.UTC(), Contract: f.Contract, Payload: string(payload)}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not store the post")
		return
	}
	if newest {
		h.forgetLevels(uid, market, day)
	}
	h.levelsPurge(ctx, now)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "market": market, "tradeDay": day, "receivedAt": now.UTC(), "newest": newest})
}

// levelsPurge drops posts older than the retention, once an hour.
func (h *Handlers) levelsPurge(ctx context.Context, now time.Time) {
	h.levelsMu.Lock()
	due := now.Sub(h.levelsPurged) > time.Hour
	if due {
		h.levelsPurged = now
	}
	h.levelsMu.Unlock()
	if due {
		_, _ = h.Queries.PurgeLevelFeeds(ctx, now.Add(-levelsFeedKeep).UTC())
	}
}

// APILevelsGet: GET /api/v1/levels/{market}?day= - the merged map.
func (h *Handlers) APILevelsGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	market := r.PathValue("market")
	day, ok := h.levelsDay(r, now)
	if !levelmap.IsMarket(market) || !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown market or bad day (YYYY-MM-DD)")
		return
	}
	m, err := h.levelMap(ctx, market, day, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "levels unavailable")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// mcpGetLevels is the MCP tool: the map of one market for a day.
func (h *Handlers) mcpGetLevels(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	now := time.Now()
	market := req.GetString("market", "")
	if root, ok := levelmap.MarketOf(market); ok {
		market = root
	}
	if !levelmap.IsMarket(market) {
		return mcpgo.NewToolResultError("market must be one of ES, NQ, YM, RTY, CL, GC (a micro's root is folded into its full-size market)"), nil
	}
	day := req.GetString("day", "")
	if day == "" || day == "today" {
		day = levelmap.TradingDay(now, h.chicago())
	} else if _, err := time.Parse("2006-01-02", day); err != nil {
		return mcpgo.NewToolResultError("day must be YYYY-MM-DD or today"), nil
	}
	m, err := h.levelMap(ctx, market, day, now)
	if err != nil {
		return mcpgo.NewToolResultError("levels unavailable: " + err.Error()), nil
	}
	return mcpJSON(m)
}
