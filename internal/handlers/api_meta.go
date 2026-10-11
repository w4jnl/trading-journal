package handlers

import (
	"bytes"
	_ "embed"
	"net/http"

	"github.com/w4jnl/trading-journal/internal/auth"
)

//go:embed api_openapi.json
var apiOpenAPISpec []byte

// apiIndexEndpoints is the hand-maintained discovery index. It is one of
// four documentation surfaces (with the OpenAPI spec, API.md and README);
// TestAPISurfaceDocsInSync keeps the first three in step.
var apiIndexEndpoints = []map[string]string{
	{"method": "GET", "path": "/api/v1/openapi.json", "summary": "OpenAPI 3 spec"},
	{"method": "GET", "path": "/api/v1/version", "summary": "Data version for change detection (poll; refetch on any change)"},
	{"method": "GET", "path": "/api/v1/stats/daily", "summary": "Daily P&L + trade stats per trading day, with range totals (the Calendar's numbers)"},
	{"method": "GET", "path": "/api/v1/analytics/summary", "summary": "Analytics for a range as one object: totals, R, drawdown, streaks, breakdowns by symbol/direction/strategy/tag/weekday/hour"},
	{"method": "GET", "path": "/api/v1/strategies", "summary": "List strategies"},
	{"method": "GET", "path": "/api/v1/strategies/{id}", "summary": "Strategy detail (name + full markdown)"},
	{"method": "GET", "path": "/api/v1/journal", "summary": "List journal entries"},
	{"method": "POST", "path": "/api/v1/journal", "summary": "Create an entry (day/week/trade/group)"},
	{"method": "POST", "path": "/api/v1/journal/prepare-day", "summary": "Cluster a day's trades + create the journal entries to analyze"},
	{"method": "GET", "path": "/api/v1/journal/{id}", "summary": "Journal entry detail"},
	{"method": "POST", "path": "/api/v1/journal/{id}/notes", "summary": "Append a dated note (append-only)"},
	{"method": "POST", "path": "/api/v1/journal/{id}/strategy", "summary": "Assign a strategy (only if the entry has none; never overwrites)"},
	{"method": "POST", "path": "/api/v1/journal/{id}/tag", "summary": "Set a tag good|neutral|bad (only if the entry has none; never overwrites)"},
	{"method": "POST", "path": "/api/v1/journal/{id}/labels", "summary": "Add mistake / emotion labels from the fixed vocabulary (additive; never removes)"},
	{"method": "GET", "path": "/api/v1/coach", "summary": "Rule-based coach checks for a trading day, with the numbers and the labels they suggest"},
	{"method": "GET", "path": "/api/v1/journal/{id}/chart.png", "summary": "Rendered chart PNG"},
	{"method": "GET", "path": "/api/v1/trades", "summary": "List trades (filtered, paged)"},
	{"method": "GET", "path": "/api/v1/trades/{id}", "summary": "Trade detail (fills + chart)"},
	{"method": "GET", "path": "/api/v1/trades/{id}/chart.png", "summary": "Rendered trade chart PNG"},
	{"method": "GET", "path": "/api/v1/attachments/{id}", "summary": "Fetch an embedded image"},
}

// APIIndex: GET /api/v1 - a discovery index (behind key auth; no anonymous access).
func (h *Handlers) APIIndex(w http.ResponseWriter, r *http.Request) {
	u := auth.MustUser(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"service":         "trading-journal-api",
		"version":         "v1",
		"authenticatedAs": map[string]any{"id": u.ID, "username": u.Username},
		"endpoints":       apiIndexEndpoints,
		"openapi":         "/api/v1/openapi.json",
	})
}

// APIOpenAPI: GET /api/v1/openapi.json - the embedded spec (behind key auth).
//
// The committed spec carries a RELATIVE server URL ("/api/v1"). Some importers -
// notably ChatGPT custom-GPT Actions - require an ABSOLUTE https URL or they refuse
// the schema. We rewrite the server to an absolute URL derived from the request
// (honoring the X-Forwarded-* headers Traefik sets), so the spec imports as-is.
// Falls back to the embedded relative URL when the host can't be determined.
func (h *Handlers) APIOpenAPI(w http.ResponseWriter, r *http.Request) {
	spec := apiOpenAPISpec
	if base := h.publicBaseURL(r); base != "" {
		spec = bytes.Replace(spec, []byte(`"url": "/api/v1"`), []byte(`"url": "`+base+`/api/v1"`), 1)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(spec)
}

// requestBaseURL reconstructs the public scheme://host the request arrived on,
// preferring the X-Forwarded-* headers a reverse proxy sets. Empty when unknown.
func requestBaseURL(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}
	scheme := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host
}
