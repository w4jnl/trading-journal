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
	{"method": "POST", "path": "/api/v1/pair", "summary": "Exchange a pairing code from Settings for a device key (no key needed; the phone app's sign-in)"},
	{"method": "GET", "path": "/api/v1/me", "summary": "The key's user, the server (version, timezone, features) and the user's settings"},
	{"method": "GET", "path": "/api/v1/today", "summary": "The day in one object: stats, open positions and unrealised P&L, the plan and how much of it is used, nudges, the curve, equity"},
	{"method": "GET", "path": "/api/v1/devices", "summary": "The phones registered for push (device key)"},
	{"method": "POST", "path": "/api/v1/devices", "summary": "Register a phone's push token with its notification settings (device key)"},
	{"method": "PUT", "path": "/api/v1/devices/{token}/prefs", "summary": "Replace a phone's notification settings (device key)"},
	{"method": "PUT", "path": "/api/v1/devices/{token}/activity", "summary": "Report a phone's push-to-start token and its running Live Activities (device key)"},
	{"method": "DELETE", "path": "/api/v1/devices/{token}", "summary": "Forget a phone (device key)"},
	{"method": "GET", "path": "/api/v1/events", "summary": "Server-sent events: a version event after every change of your data (refetch /today), a comment every 25 s"},
	{"method": "GET", "path": "/api/v1/nudges", "summary": "A day's guardrail nudges with their seen state and the app route each leads to"},
	{"method": "POST", "path": "/api/v1/nudges/seen", "summary": "Mark several nudges seen"},
	{"method": "POST", "path": "/api/v1/nudges/{id}/seen", "summary": "Mark a nudge seen (the trader acknowledged it)"},
	{"method": "PUT", "path": "/api/v1/settings", "summary": "Change the user's switches: guardrail, automatic plans (device key)"},
	{"method": "GET", "path": "/api/v1/stats/daily", "summary": "Daily P&L + trade stats per trading day, with range totals (the Calendar's numbers)"},
	{"method": "GET", "path": "/api/v1/analytics/summary", "summary": "Analytics for a range as one object: totals, R, drawdown, streaks, breakdowns by symbol/direction/strategy/tag/weekday/hour"},
	{"method": "GET", "path": "/api/v1/strategies", "summary": "List strategies"},
	{"method": "GET", "path": "/api/v1/strategies/{id}", "summary": "Strategy detail (name + full markdown)"},
	{"method": "GET", "path": "/api/v1/journal", "summary": "List journal entries"},
	{"method": "POST", "path": "/api/v1/journal", "summary": "Create an entry (day/week/trade/group)"},
	{"method": "POST", "path": "/api/v1/journal/prepare-day", "summary": "Cluster a day's trades + create the journal entries to analyze"},
	{"method": "GET", "path": "/api/v1/journal/labels", "summary": "The mistake and emotion vocabulary with display names"},
	{"method": "POST", "path": "/api/v1/journal/attachments", "summary": "Upload an image for a journal body (device key, multipart)"},
	{"method": "GET", "path": "/api/v1/journal/{id}", "summary": "Journal entry detail"},
	{"method": "PUT", "path": "/api/v1/journal/{id}", "summary": "Replace the entry's body, tag, mood, strategy, labels and checklist (device key; the trader's own edit)"},
	{"method": "DELETE", "path": "/api/v1/journal/{id}", "summary": "Delete the entry (device key)"},
	{"method": "POST", "path": "/api/v1/journal/{id}/notes", "summary": "Append a dated note (append-only)"},
	{"method": "POST", "path": "/api/v1/journal/{id}/strategy", "summary": "Assign a strategy (only if the entry has none; never overwrites)"},
	{"method": "POST", "path": "/api/v1/journal/{id}/tag", "summary": "Set a tag good|neutral|bad (only if the entry has none; never overwrites)"},
	{"method": "POST", "path": "/api/v1/journal/{id}/labels", "summary": "Add mistake / emotion labels from the fixed vocabulary (additive; never removes)"},
	{"method": "POST", "path": "/api/v1/journal/{id}/suggestions", "summary": "Accept or dismiss an AI review's suggestion on the entry (device key)"},
	{"method": "GET", "path": "/api/v1/coach", "summary": "Rule-based coach checks for a trading day, with the numbers and the labels they suggest"},
	{"method": "GET", "path": "/api/v1/plan", "summary": "The pre-market plan of a day and how the day measured against it"},
	{"method": "GET", "path": "/api/v1/digest", "summary": "The weekly edge digest: an ISO week per decision against the week before, what to keep and fix, coach and plan tallies"},
	{"method": "POST", "path": "/api/v1/plan", "summary": "Write or amend the plan for today or a future day (present fields replace, absent ones are kept)"},
	{"method": "GET", "path": "/api/v1/journal/{id}/chart.png", "summary": "Rendered chart PNG"},
	{"method": "GET", "path": "/api/v1/trades", "summary": "List trades (filtered, paged)"},
	{"method": "GET", "path": "/api/v1/trades/{id}", "summary": "Trade detail (fills + chart)"},
	{"method": "GET", "path": "/api/v1/trades/{id}/chart.png", "summary": "Rendered trade chart PNG"},
	{"method": "GET", "path": "/api/v1/accounts", "summary": "Accounts with their running balance (from the balance anchors) and today's net"},
	{"method": "POST", "path": "/api/v1/accounts/{id}/balances", "summary": "Record the account's balance on a day (device key)"},
	{"method": "DELETE", "path": "/api/v1/accounts/{id}/balances/{anchorId}", "summary": "Remove a balance anchor (device key)"},
	{"method": "GET", "path": "/api/v1/equity", "summary": "Equity now by account type, the per-day curve, the drawdown and today's change"},
	{"method": "GET", "path": "/api/v1/positions", "summary": "Open positions with their plans"},
	{"method": "POST", "path": "/api/v1/positions/{account}/{symbol}/plan", "summary": "Record the plan on an open position"},
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
