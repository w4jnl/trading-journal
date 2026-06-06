package handlers

import (
	_ "embed"
	"net/http"

	"github.com/w4jnl/trading-journal/internal/auth"
)

//go:embed api_openapi.json
var apiOpenAPISpec []byte

// APIIndex: GET /api/v1 - a discovery index (behind key auth; no anonymous access).
func (h *Handlers) APIIndex(w http.ResponseWriter, r *http.Request) {
	u := auth.MustUser(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"service":         "trading-journal-api",
		"version":         "v1",
		"authenticatedAs": map[string]any{"id": u.ID, "username": u.Username},
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/api/v1/openapi.json", "summary": "OpenAPI 3 spec"},
			{"method": "GET", "path": "/api/v1/strategies", "summary": "List strategies"},
			{"method": "GET", "path": "/api/v1/strategies/{id}", "summary": "Strategy detail (name + full markdown)"},
			{"method": "GET", "path": "/api/v1/journal", "summary": "List journal entries"},
			{"method": "GET", "path": "/api/v1/journal/{id}", "summary": "Journal entry detail"},
			{"method": "POST", "path": "/api/v1/journal/{id}/notes", "summary": "Append a dated note (append-only)"},
			{"method": "GET", "path": "/api/v1/journal/{id}/chart.png", "summary": "Rendered chart PNG"},
			{"method": "GET", "path": "/api/v1/attachments/{id}", "summary": "Fetch an embedded image"},
		},
		"openapi": "/api/v1/openapi.json",
	})
}

// APIOpenAPI: GET /api/v1/openapi.json - the embedded spec (behind key auth).
func (h *Handlers) APIOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(apiOpenAPISpec)
}
