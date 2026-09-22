package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// Release builds serve /static with an immutable year-long cache and an ETag
// that turns a revalidation into a 304; dev builds are served no-cache.
func TestStaticCacheHeaders(t *testing.T) {
	fsys := fstest.MapFS{"css/app.css": {Data: []byte("body{}")}}
	serve := func(version string, r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		staticCacheHeaders(version, http.FileServer(http.FS(fsys))).ServeHTTP(rec, r)
		return rec
	}

	rec := serve("v2.11.2", httptest.NewRequest("GET", "/css/app.css?v=v2.11.2", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "body{}" {
		t.Fatalf("release GET: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") != `"v2.11.2"` || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("release headers: %v", rec.Header())
	}

	req := httptest.NewRequest("GET", "/css/app.css?v=v2.11.2", nil)
	req.Header.Set("If-None-Match", `"v2.11.2"`)
	if rec = serve("v2.11.2", req); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revalidation: %d body=%d, want 304 empty", rec.Code, rec.Body.Len())
	}
	req.Header.Set("If-None-Match", `"v2.11.1"`)
	if rec = serve("v2.11.2", req); rec.Code != http.StatusOK {
		t.Errorf("stale ETag must be served fresh, got %d", rec.Code)
	}

	rec = serve("", httptest.NewRequest("GET", "/css/app.css", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") != "" {
		t.Errorf("dev headers: %d %v", rec.Code, rec.Header())
	}
}
