package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// gexPageGet routes a request through a real mux so r.PathValue works.
func gexPageGet(t *testing.T, h *Handlers, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gex", h.GEXPage)
	mux.HandleFunc("GET /gex/{market}", h.GEXPage)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester", DisplayName: "Tester"})
	req := httptest.NewRequest("GET", target, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestGEXPage(t *testing.T) {
	db := gexTestDB(t)
	var calls atomic.Int64
	var lastPath, lastAt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastPath, lastAt = r.URL.Path, r.URL.Query().Get("at")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02Z"))
	}))
	defer srv.Close()

	c, err := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	// Default market is ES; the page carries tiles, chart JSON, and levels.
	rec := gexPageGet(t, h, "/gex")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /gex = %d", rec.Code)
	}
	if lastPath != "/v1/snapshots/ES" {
		t.Errorf("engine path = %q", lastPath)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"positive",      // regime tile
		"&#43;74.5B",    // net gex tile (html/template escapes the plus)
		"7465.79",       // flip futures
		"Call wall",     // levels table
		`const gexData`, // chart blob
		`"strikes":[7300,7530]`,
		"ET", // as-of label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Market path routes through to the engine; junk markets 404 locally.
	gexPageGet(t, h, "/gex/NQ")
	if lastPath != "/v1/snapshots/NQ" {
		t.Errorf("engine path for /gex/NQ = %q", lastPath)
	}
	if rec := gexPageGet(t, h, "/gex/CL"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /gex/CL = %d, want 404", rec.Code)
	}

	// ?at= rewinds: the engine sees the UTC instant, the page loses the
	// auto-refresh and offers a Latest link.
	before := calls.Load()
	rec = gexPageGet(t, h, "/gex?at=2026-07-31T10:54")
	if calls.Load() != before+1 {
		t.Fatalf("rewind did not hit the engine")
	}
	if lastAt != "2026-07-31T10:54:00Z" {
		t.Errorf("engine at = %q (tz = UTC in this harness)", lastAt)
	}
	body = rec.Body.String()
	if !strings.Contains(body, ">Latest<") {
		t.Errorf("rewind view missing the Latest link")
	}
	if strings.Contains(body, "location.reload") {
		t.Errorf("rewind view must not auto-refresh")
	}
}

func TestGEXPageDisabledAndErrors(t *testing.T) {
	db := gexTestDB(t)

	// Nil client: feature off, route 404s.
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC}
	if rec := gexPageGet(t, h, "/gex"); rec.Code != http.StatusNotFound {
		t.Errorf("nil GEX: GET /gex = %d, want 404", rec.Code)
	}

	// Engine 404: empty state, still 200.
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"none"}`, http.StatusNotFound)
	}))
	defer srv404.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv404.URL})
	h.GEX = c
	rec := gexPageGet(t, h, "/gex?at=2026-07-01T10:00")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No snapshot at or before") {
		t.Errorf("engine 404: code %d, empty state missing", rec.Code)
	}

	// Engine down: error flash, still 200, shell intact.
	srvDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvDown.Close()
	c2, _ := gex.New(gex.Options{BaseURL: srvDown.URL})
	h.GEX = c2
	rec = gexPageGet(t, h, "/gex")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "gex-engine unreachable") {
		t.Errorf("engine down: code %d, flash missing", rec.Code)
	}
	if !strings.Contains(body, "Gamma (GEX)") {
		t.Errorf("engine down: page shell missing")
	}
}

func TestGexFmtB(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{74486642363.049, "+74.5B"},
		{-3897046844, "-3.9B"},
		{0, "+0.0B"},
		{250e6, "+250M"},
		{-1200, "-1200"},
	}
	for _, c := range cases {
		if got := gexFmtB(c.v); got != c.want {
			t.Errorf("gexFmtB(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}
