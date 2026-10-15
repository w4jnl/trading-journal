package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/w4jnl/trading-journal/internal/guardrail"
	"github.com/w4jnl/trading-journal/internal/markethours"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- The day for the phone: nudges, the Today aggregate, the event stream --
//
// The phone's Today screen, its widgets and its Live Activity all read one
// object, GET /api/v1/today: the day's stats, what is open with its
// unrealised P&L, the plan with how much of it is used, the nudges, the
// last fill, the cumulative curve and the equity. GET /api/v1/events is
// the live signal behind it: an SSE stream that says "version N" after
// every committed change (livedata.Notifier.Subscribe), so the app
// refetches /today instead of polling. Nudges get a "seen" of their own:
// delivered_at keeps meaning a web page took the nudge, seen_at that the
// trader acknowledged it anywhere; the phone's badge is the unseen count.

// nudgeStaleForBadge bounds the badge: a nudge older than this has passed.
const nudgeStaleForBadge = 2 * time.Hour

// apiNudgeFrom projects a nudge row.
func apiNudgeFrom(n models.Nudge) map[string]any {
	return map[string]any{
		"id": n.ID, "day": n.Day, "key": n.Key, "ref": n.Ref, "severity": n.Severity,
		"title": n.Title, "detail": n.Detail, "lesson": n.Lesson, "href": n.Href, "route": guardrail.RouteFor(n.Href),
		"createdAt": n.CreatedAt, "deliveredAt": n.DeliveredAt, "seenAt": n.SeenAt,
	}
}

// nudgesOfDay lists a day's nudges newest first with the unseen count.
func (h *Handlers) nudgesOfDay(r *http.Request, day string) ([]map[string]any, int64, error) {
	ctx := r.Context()
	rows, err := h.rq().ListNudgesForDayDesc(ctx, models.ListNudgesForDayDescParams{UserID: userID(ctx), Day: day})
	if err != nil {
		return nil, 0, err
	}
	items := make([]map[string]any, 0, len(rows))
	var unseen int64
	for _, n := range rows {
		items = append(items, apiNudgeFrom(n))
		if n.SeenAt == nil {
			unseen++
		}
	}
	return items, unseen, nil
}

// APINudgesList: GET /api/v1/nudges?day=today|YYYY-MM-DD
func (h *Handlers) APINudgesList(w http.ResponseWriter, r *http.Request) {
	tz := h.localTZ()
	day := h.resolveDayKeyword(r.Context(), r.URL.Query().Get("day"), tz)
	if day == "" {
		day = todayLocal(tz).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad day; expected YYYY-MM-DD or today|yesterday|last")
		return
	}
	items, unseen, err := h.nudgesOfDay(r, day)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not list nudges")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"day": day, "items": items, "unseen": unseen})
}

// APINudgeSeen: POST /api/v1/nudges/{id}/seen - the trader acknowledged it.
func (h *Handlers) APINudgeSeen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	n, err := h.Queries.MarkNudgeSeen(ctx, models.MarkNudgeSeenParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not mark the nudge")
		return
	}
	if n > 0 {
		h.LiveData.Bump(userID(ctx))
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "seen": n > 0})
}

// APINudgesSeen: POST /api/v1/nudges/seen {"ids": [...]} - several at once.
func (h *Handlers) APINudgesSeen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || len(in.IDs) == 0 {
		writeJSONError(w, http.StatusBadRequest, "ids is required")
		return
	}
	n, err := h.Queries.MarkNudgesSeen(ctx, models.MarkNudgesSeenParams{UserID: userID(ctx), Ids: in.IDs})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not mark the nudges")
		return
	}
	if n > 0 {
		h.LiveData.Bump(userID(ctx))
	}
	writeJSON(w, http.StatusOK, map[string]any{"seen": n})
}

// unseenBadge is the phone's badge: today's unseen nudges from the last
// two hours.
func (h *Handlers) unseenBadge(r *http.Request, day string) int64 {
	return h.unseenBadgeCtx(r.Context(), day)
}

func (h *Handlers) unseenBadgeCtx(ctx context.Context, day string) int64 {
	n, err := h.rq().CountUnseenNudges(ctx, models.CountUnseenNudgesParams{UserID: userID(ctx), Day: day, CreatedAt: time.Now().UTC().Add(-nudgeStaleForBadge)})
	if err != nil {
		return 0
	}
	return n
}

// APIToday: GET /api/v1/today[?refresh=1] - the day in one object.
func (h *Handlers) APIToday(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.syncForAPIRead(ctx, apiWantsRefresh(r))
	out, err := h.todayJSON(r)
	if err != nil {
		h.logger().Error("api today", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not build today")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// todayJSON builds the Today object.
func (h *Handlers) todayJSON(r *http.Request) (map[string]any, error) {
	ctx := r.Context()
	tz := h.localTZ()
	now := time.Now()
	today := todayLocal(tz)
	dayKey := today.Format("2006-01-02")
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		chicago = tz
	}
	ct := now.In(chicago)
	rthStart := time.Date(ct.Year(), ct.Month(), ct.Day(), 8, 30, 0, 0, chicago)
	rthEnd := time.Date(ct.Year(), ct.Month(), ct.Day(), 15, 0, 0, 0, chicago)

	// The day's stats.
	stats, err := h.dailyStats(ctx, accountFilter{}, apiDateRange{From: today, To: today})
	if err != nil {
		return nil, err
	}

	// What is open, and the unrealised P&L across it.
	positions, err := h.openPositionsAPI(ctx)
	if err != nil {
		return nil, err
	}
	var unreal float64
	priced, unpriced := 0, 0
	stale := false
	for _, p := range positions {
		if p.Unrealized == nil {
			unpriced++
			continue
		}
		priced++
		unreal += *p.Unrealized
		if p.LastAt == nil || now.Sub(*p.LastAt) > 5*time.Minute {
			stale = true
		}
	}

	// The day's closed trades, in exit order: the curve and the last one.
	from, to := today.UTC(), today.AddDate(0, 0, 1).UTC()
	trades, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, &from, &to)
	if err != nil {
		return nil, err
	}
	curve := make([]map[string]any, 0, len(trades))
	var cum float64
	var lastTrade map[string]any
	for _, t := range trades {
		if t.NetPnl == nil {
			continue
		}
		cum += *t.NetPnl
		curve = append(curve, map[string]any{"t": t.ExitTime, "net": cum})
		lastTrade = map[string]any{"id": t.ID, "symbolRoot": t.SymbolRoot, "direction": t.Direction, "netPnl": *t.NetPnl, "exitTime": t.ExitTime, "account": t.AccountName}
	}

	// The plan and how much of it is used: decisions against maxTrades,
	// the worst account's realised loss against maxLoss.
	dayNet := h.dayNetByAccount(r, today)
	planOut := map[string]any{"plan": nil, "outcome": nil, "tradesUsed": 0, "maxTrades": nil, "lossUsed": 0.0, "maxLoss": nil, "worstAccount": ""}
	if rep := h.coachReport(ctx, today); rep != nil {
		planOut["tradesUsed"] = rep.Decisions
		if rep.Plan != nil {
			planOut["plan"] = rep.Plan.Plan
			planOut["outcome"] = rep.Plan
			planOut["maxTrades"] = rep.Plan.Plan.MaxTrades
			planOut["maxLoss"] = rep.Plan.Plan.MaxLoss
		}
	}
	if p := h.dayPlan(ctx, dayKey); p != nil && planOut["plan"] == nil {
		planOut["plan"] = p
		planOut["maxTrades"] = p.MaxTrades
		planOut["maxLoss"] = p.MaxLoss
	}
	names := map[int64]string{}
	if accounts, err := h.rq().ListAccounts(ctx, userID(ctx)); err == nil {
		for _, a := range accounts {
			names[a.ID] = a.Name
		}
	}
	var worst float64
	for id, net := range dayNet {
		if net < worst {
			worst = net
			planOut["worstAccount"] = names[id]
		}
	}
	if worst < 0 {
		planOut["lossUsed"] = -worst
	}

	// Nudges, newest first.
	nudgeItems, unseen, err := h.nudgesOfDay(r, dayKey)
	if err != nil {
		return nil, err
	}
	if len(nudgeItems) > 10 {
		nudgeItems = nudgeItems[:10]
	}

	// Equity: the total now by type, today's change, the drawdown now.
	equity := map[string]any{"now": 0.0, "byType": []any{}, "dayChange": 0.0, "currentDrawdown": 0.0, "underwater": false, "known": false}
	if _, _, equityNow, byType, included, _, err := h.balanceSeriesForFilter(ctx, accountFilter{}, nil, nil, tz); err == nil && included > 0 {
		types := make([]map[string]any, 0, len(byType))
		for _, t := range byType {
			types = append(types, map[string]any{"type": t.Type, "label": t.Label, "total": t.Total, "accounts": t.Accounts})
		}
		equity["now"], equity["byType"], equity["known"] = equityNow, types, true
	}
	if accounts, err := h.rq().ListAccounts(ctx, userID(ctx)); err == nil {
		var change float64
		for _, a := range accounts {
			if a.Active == 1 && a.ExcludedFromStats == 0 {
				change += dayNet[a.ID]
			}
		}
		equity["dayChange"] = change
	}
	if all, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, nil, nil); err == nil {
		dd := computeDrawdown(all)
		equity["currentDrawdown"], equity["underwater"] = dd.CurrentDD, dd.Underwater
	}

	// The last fill: the newest fill of what is open, or the last close.
	var lastFill *time.Time
	for _, p := range positions {
		for _, f := range p.CycleFills {
			if lastFill == nil || f.Time.After(*lastFill) {
				t := f.Time
				lastFill = &t
			}
		}
	}
	if lastTrade != nil {
		if t, ok := lastTrade["exitTime"].(time.Time); ok && (lastFill == nil || t.After(*lastFill)) {
			lastFill = &t
		}
	}

	return map[string]any{
		"day":      dayKey,
		"timezone": tz.String(),
		"v":        h.LiveData.Version(userID(ctx)),
		"asOf":     now.UTC(),
		"session": map[string]any{
			"rthStart": rthStart.UTC(), "rthEnd": rthEnd.UTC(),
			"inGlobex": markethours.InGlobexHours(now, chicago),
			"inRth":    markethours.InRTHWindow(now, chicago, 8*60+30, 15*60),
		},
		"stats":      stats.Totals,
		"unrealized": map[string]any{"total": unreal, "stale": stale, "priced": priced, "unpriced": unpriced},
		"positions":  positions,
		"plan":       planOut,
		"nudges":     map[string]any{"unseen": unseen, "badge": h.unseenBadge(r, dayKey), "items": nudgeItems},
		"lastFillAt": lastFill,
		"lastTrade":  lastTrade,
		"curve":      curve,
		"equity":     equity,
	}, nil
}

// APIEvents: GET /api/v1/events - a server-sent event stream: "version"
// events after every committed change of the user's data (the value to
// compare, never to count), a comment every 25 s to keep the connection
// alive. Mounted outside the server's write timeout.
func (h *Handlers) APIEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming is not supported here")
		return
	}
	ctx := r.Context()
	uid := userID(ctx)
	h.nudgeSyncFromAPI(ctx)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ch, cancel := h.LiveData.Subscribe(uid)
	defer cancel()
	writeVersion := func() bool {
		_, err := fmt.Fprintf(w, "event: version\ndata: {\"v\":%s}\n\n", strconv.FormatUint(h.LiveData.Version(uid), 10))
		fl.Flush()
		return err == nil
	}
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	if !writeVersion() {
		return
	}
	ping := time.NewTicker(sseKeepAlive)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if !writeVersion() {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// sseKeepAlive is the comment interval; a var so tests can shorten it.
var sseKeepAlive = 25 * time.Second
