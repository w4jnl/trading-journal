package handlers

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// APIMux builds the /api/v1 route table. It's mounted at "/api/" in main.go and
// wrapped by auth.APIKeyAuth.Middleware, which injects the key's user into the
// request context - so every handler below can use userID(ctx) and reuse the
// same user-scoped loaders the HTML app uses. Patterns are the FULL path
// because http.ServeMux subtree dispatch does not strip the prefix.
func (h *Handlers) APIMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1", h.APIIndex)
	m.HandleFunc("GET /api/v1/openapi.json", h.APIOpenAPI)
	// Change detection for consumer apps: the same in-memory version the
	// HTML pages poll, reachable with a key (the /data/version route sits
	// behind the session chain) - plus the read-triggered sync nudge.
	m.HandleFunc("GET /api/v1/version", h.APIVersion)
	m.HandleFunc("GET /api/v1/me", h.APIMe)
	m.HandleFunc("GET /api/v1/today", h.APIToday)
	m.HandleFunc("GET /api/v1/devices", h.APIDevicesList)
	m.HandleFunc("POST /api/v1/devices", h.APIDeviceRegister)
	m.HandleFunc("PUT /api/v1/devices/{token}/prefs", h.APIDevicePrefs)
	m.HandleFunc("PUT /api/v1/devices/{token}/activity", h.APIDeviceActivity)
	m.HandleFunc("DELETE /api/v1/devices/{token}", h.APIDeviceDelete)
	m.HandleFunc("GET /api/v1/nudges", h.APINudgesList)
	m.HandleFunc("POST /api/v1/nudges/seen", h.APINudgesSeen)
	m.HandleFunc("POST /api/v1/nudges/{id}/seen", h.APINudgeSeen)
	// GET /api/v1/events is mounted in main.go, outside the write timeout.
	m.HandleFunc("PUT /api/v1/settings", h.APISettingsSave)
	m.HandleFunc("GET /api/v1/stats/daily", h.APIStatsDaily)
	m.HandleFunc("GET /api/v1/analytics/summary", h.APIAnalyticsSummary)
	m.HandleFunc("GET /api/v1/strategies", h.APIStrategiesList)
	m.HandleFunc("GET /api/v1/strategies/{id}", h.APIStrategyDetail)
	m.HandleFunc("GET /api/v1/journal", h.APIJournalList)
	m.HandleFunc("POST /api/v1/journal", h.APIJournalCreate)
	m.HandleFunc("POST /api/v1/journal/prepare-day", h.APIPrepareDay)
	m.HandleFunc("GET /api/v1/journal/labels", h.APIJournalLabels)
	m.HandleFunc("POST /api/v1/journal/attachments", h.APIJournalAttachmentUpload)
	m.HandleFunc("GET /api/v1/journal/{id}", h.APIJournalDetail)
	m.HandleFunc("PUT /api/v1/journal/{id}", h.APIJournalUpdate)
	m.HandleFunc("DELETE /api/v1/journal/{id}", h.APIJournalDelete)
	m.HandleFunc("POST /api/v1/journal/{id}/notes", h.APIJournalAppendNote)
	m.HandleFunc("POST /api/v1/journal/{id}/strategy", h.APIJournalAssignStrategy)
	m.HandleFunc("POST /api/v1/journal/{id}/tag", h.APIJournalAssignTag)
	m.HandleFunc("POST /api/v1/journal/{id}/labels", h.APIJournalAddLabels)
	m.HandleFunc("POST /api/v1/journal/{id}/suggestions", h.APIJournalSuggestion)
	m.HandleFunc("GET /api/v1/coach", h.APICoachChecks)
	m.HandleFunc("GET /api/v1/plan", h.APIDayPlan)
	m.HandleFunc("GET /api/v1/digest", h.APIDigest)
	m.HandleFunc("POST /api/v1/plan", h.APIDayPlanSave)
	m.HandleFunc("GET /api/v1/journal/{id}/chart.png", h.APIJournalChartPNG)
	m.HandleFunc("GET /api/v1/trades", h.APITradesList)
	m.HandleFunc("GET /api/v1/trades/{id}", h.APITradeDetail)
	m.HandleFunc("GET /api/v1/trades/{id}/chart.png", h.APITradeChartPNG)
	m.HandleFunc("GET /api/v1/accounts", h.APIAccountsList)
	m.HandleFunc("POST /api/v1/accounts/{id}/balances", h.APIAccountBalanceAdd)
	m.HandleFunc("DELETE /api/v1/accounts/{id}/balances/{anchorId}", h.APIAccountBalanceDelete)
	m.HandleFunc("GET /api/v1/equity", h.APIEquity)
	m.HandleFunc("GET /api/v1/positions", h.APIPositionsList)
	m.HandleFunc("POST /api/v1/positions/{account}/{symbol}/plan", h.APIPositionPlanSave)
	m.HandleFunc("GET /api/v1/attachments/{id}", h.APIAttachmentServe)
	return m
}

// nudgeSyncFromAPI asks both broker syncs to refresh the caller's data, the
// way a page visit does - so a consumer app reading trades/stats/version
// keeps the data moving without anyone opening a web page. Throttled per
// user by api_sync_throttle_sec (the services' APIThrottle; 0 disables) and
// nil-safe when a broker is off. The sync is asynchronous: the current read
// returns what is there now, and new fills bump /api/v1/version shortly
// after - which is exactly the poll-then-refetch loop consumers already run.
func (h *Handlers) nudgeSyncFromAPI(ctx context.Context) {
	uid := userID(ctx)
	h.TopstepxSync.MaybeEnqueueUserAPI(uid)
	h.IbkrSync.MaybeEnqueueUserAPI(uid)
}

// APIVersion: GET /api/v1/version - DataVersion plus the sync nudge, so a
// consumer's cheap change-detection poll doubles as its presence signal.
func (h *Handlers) APIVersion(w http.ResponseWriter, r *http.Request) {
	h.nudgeSyncFromAPI(r.Context())
	h.DataVersion(w, r)
}

// apiRefreshWait bounds how long a refresh read waits for the nudged sync.
// A var so tests can shorten it.
var apiRefreshWait = 15 * time.Second

// refreshSyncFromAPI is the refresh variant of nudgeSyncFromAPI: nudge, then
// wait - bounded, one shared deadline across both brokers - until the
// caller's syncs are idle, so the read that follows sees the fills the nudge
// pulled ("what is my P&L right now" gets a current answer, not the last
// poll's). Within the API throttle nothing is enqueued and this returns at
// once, so a refresh read is never slower than the sync it actually caused.
func (h *Handlers) refreshSyncFromAPI(ctx context.Context) {
	h.nudgeSyncFromAPI(ctx)
	uid := userID(ctx)
	deadline := time.Now().Add(apiRefreshWait)
	h.TopstepxSync.WaitForUser(ctx, uid, deadline)
	h.IbkrSync.WaitForUser(ctx, uid, deadline)
}

// syncForAPIRead runs a read's sync side effect: refresh (nudge + wait) when
// asked, a plain nudge otherwise.
func (h *Handlers) syncForAPIRead(ctx context.Context, refresh bool) {
	if refresh {
		h.refreshSyncFromAPI(ctx)
		return
	}
	h.nudgeSyncFromAPI(ctx)
}

// apiWantsRefresh reads the shared ?refresh=1 flag.
func apiWantsRefresh(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("refresh"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// resolveDayKeyword maps the date keywords the API and MCP tools accept onto
// YYYY-MM-DD in the trading tz: today, yesterday (calendar days) and last
// (the last trading day before today with closed trades - the same
// resolution prepare_day uses; falls back to yesterday when there is no
// history). Anything else passes through untouched to the normal date
// parsing, so a typo still fails as a bad date instead of silently meaning
// something. This exists because the caller is usually a model that does
// not reliably know today's date in the user's timezone.
func (h *Handlers) resolveDayKeyword(ctx context.Context, raw string, tz *time.Location) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "today":
		return todayLocal(tz).Format("2006-01-02")
	case "yesterday":
		return todayLocal(tz).AddDate(0, 0, -1).Format("2006-01-02")
	case "last":
		// lastTradingDay reports "no history" as a zero time, not an error.
		if d, err := h.lastTradingDay(ctx, tz); err == nil && !d.IsZero() {
			return d.In(tz).Format("2006-01-02")
		}
		return todayLocal(tz).AddDate(0, 0, -1).Format("2006-01-02")
	}
	return raw
}

// ---- API response projections (camelCase; we never serialize sqlc model
// structs directly - they're snake_case and models.User carries password_hash) ----

type apiAttachment struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

type apiStrategy struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Body        string          `json:"body"`
	Attachments []apiAttachment `json:"attachments"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

type apiStrategyRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type apiTrade struct {
	ID         int64      `json:"id"`
	Symbol     string     `json:"symbol"`
	SymbolRoot string     `json:"symbolRoot"`
	Direction  string     `json:"direction"`
	Qty        float64    `json:"qty"`
	Account    string     `json:"account"`
	EntryTime  time.Time  `json:"entryTime"`
	ExitTime   *time.Time `json:"exitTime"`
	GrossPnl   *float64   `json:"grossPnl"`
	Commission *float64   `json:"commission"`
	NetPnl     *float64   `json:"netPnl"`
}

type apiSummary struct {
	Trades     int64   `json:"trades"`
	Net        float64 `json:"net"`
	Gross      float64 `json:"gross"`
	Commission float64 `json:"commission"`
	Wins       int64   `json:"wins"`
	Losses     int64   `json:"losses"`
}

func apiTradesFrom(trades []linkedTrade) []apiTrade {
	out := make([]apiTrade, 0, len(trades))
	for _, t := range trades {
		out = append(out, apiTrade{
			ID: t.ID, Symbol: t.Symbol, SymbolRoot: t.SymbolRoot, Direction: t.Direction,
			Qty: t.Qty, Account: t.AccountName, EntryTime: t.EntryTime, ExitTime: t.ExitTime,
			GrossPnl: t.GrossPnl, Commission: t.Commission, NetPnl: t.NetPnl,
		})
	}
	return out
}

func apiSummaryFrom(s GroupSummary) apiSummary {
	return apiSummary{Trades: s.N, Net: s.Net, Gross: s.Gross, Commission: s.Commission, Wins: s.Wins, Losses: s.Losses}
}

// attachmentRefRe matches the /journal/attachments/{id} URLs embedded in journal
// and strategy markdown bodies.
var attachmentRefRe = regexp.MustCompile(`/journal/attachments/(\d+)`)

// apiRewriteAttachmentRefs rewrites in-body image refs to the API path
// (/api/v1/attachments/{id}) and returns the rewritten markdown plus the deduped
// referenced attachment ids in first-seen order. Used for both strategy bodies
// and journal notes. Body refs are the only reliable source of an item's images:
// journal_attachments.entry_id is left 0 by the save handlers and strategies have
// no attachment FK at all.
func apiRewriteAttachmentRefs(md string) (string, []int64) {
	var ids []int64
	seen := map[int64]bool{}
	out := attachmentRefRe.ReplaceAllStringFunc(md, func(m string) string {
		idStr := m[len("/journal/attachments/"):]
		id, _ := strconv.ParseInt(idStr, 10, 64)
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		return "/api/v1/attachments/" + idStr
	})
	return out, ids
}

func apiAttachmentsFrom(ids []int64) []apiAttachment {
	out := make([]apiAttachment, 0, len(ids))
	for _, id := range ids {
		out = append(out, apiAttachment{ID: id, URL: "/api/v1/attachments/" + strconv.FormatInt(id, 10)})
	}
	return out
}

// apiPathID parses the {id} path value as an int64.
func apiPathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
