package handlers

import (
	"context"
	"encoding/json"
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

// The page embeds the scrub grid + slider whenever the engine window has
// slots (it always does once history exists).
func TestGEXPageScrubSlider(t *testing.T) {
	db := gexTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02Z"))
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	body := gexPageGet(t, h, "/gex").Body.String()
	for _, want := range []string{`id="gx-scrub"`, "const grid =", "/gex/data/ES?at="} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// Denominations must never be ambiguous on the page: the flip is shown in
// BOTH index and futures terms, every spot-relative distance is computed
// futures-vs-futures, and the profile chart's reference lines stay in index
// terms (it plots index strikes). A reviewer reading one unlabeled flip
// number next to an index-led spot tile previously mistook this for a unit
// bug - the numbers were right, the labeling was not.
func TestGEXPageDenominations(t *testing.T) {
	db := gexTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02Z"))
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	// Fixture: spot_index 7494.55, ratio 1.0038 -> spot futures 7523.03;
	// flip index 7437.55, flip futures 7465.788539891167.
	body := gexPageGet(t, h, "/gex").Body.String()
	for _, want := range []string{
		"Spot · index",         // tile states its own denomination
		"Gamma flip · futures", // ...and so does the flip
		"index 7437.55",        // flip's index value, the one the chart plots
		"7465.79",              // flip's futures value (tile + ladder row)
		"Gamma flip</td>",      // flip listed in the level ladder
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}

	// The distance must be futures-minus-futures (7523.03 - 7465.79 =
	// +57.24). Mixing units against the INDEX flip would read +85.48.
	if !strings.Contains(body, "spot &#43;57.24 pts (above flip)") {
		t.Errorf("flip distance is not futures-vs-futures")
	}
	if strings.Contains(body, "85.48") {
		t.Errorf("flip distance mixed futures spot with index flip")
	}

	// The chart plots index strikes, so its reference lines must be index
	// values - the flip line at 7437.55, never the futures 7465.79.
	i := strings.Index(body, "const gexInit")
	blob := body[i : i+strings.Index(body[i:], "\n")]
	if !strings.Contains(blob, `"flip":7437.55`) || !strings.Contains(blob, `"spot":7494.55`) {
		t.Errorf("chart reference lines are not index-denominated: %s", blob)
	}
}

func TestGEXData(t *testing.T) {
	db := gexTestDB(t)
	var lastAt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAt = r.URL.Query().Get("at")
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02Z"))
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	get := func(target string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /gex/data/{market}", h.GEXData)
		ctx := auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester", DisplayName: "Tester"})
		req := httptest.NewRequest("GET", target, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Happy path: unix at forwarded as the UTC instant, full view returned.
	rec := get("/gex/data/ES?at=1785526202") // 2026-07-31T19:30:02Z
	if rec.Code != http.StatusOK {
		t.Fatalf("data = %d", rec.Code)
	}
	if lastAt != "2026-07-31T19:30:02Z" {
		t.Errorf("engine at = %q", lastAt)
	}
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v["regime"] != "positive" || v["netGex"] != "+74.5B" {
		t.Errorf("view = regime %v netGex %v", v["regime"], v["netGex"])
	}
	chart, _ := v["chart"].(map[string]any)
	if strikes, _ := chart["strikes"].([]any); len(strikes) != 2 {
		t.Errorf("chart strikes = %v", chart["strikes"])
	}

	// Bad inputs 400/404 locally, without engine calls.
	if rec := get("/gex/data/ES?at=abc"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad at = %d", rec.Code)
	}
	if rec := get("/gex/data/CL?at=1785526202"); rec.Code != http.StatusNotFound {
		t.Errorf("bad market = %d", rec.Code)
	}
	h.GEX = nil
	if rec := get("/gex/data/ES?at=1785526202"); rec.Code != http.StatusNotFound {
		t.Errorf("nil client = %d", rec.Code)
	}

	// Engine 404 is a normal empty state; transport trouble is a 502.
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"none"}`, http.StatusNotFound)
	}))
	defer srv404.Close()
	h.GEX, _ = gex.New(gex.Options{BaseURL: srv404.URL})
	rec = get("/gex/data/ES?at=1785526202")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"empty":true`) {
		t.Errorf("engine 404: code %d body %s", rec.Code, rec.Body.String())
	}
	srvDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvDown.Close()
	h.GEX, _ = gex.New(gex.Options{BaseURL: srvDown.URL})
	if rec := get("/gex/data/ES?at=1785526202"); rec.Code != http.StatusBadGateway {
		t.Errorf("engine down = %d", rec.Code)
	}
}

// gexSessionGrid: ET session slots on weekdays only, ascending, bounded by
// the engine history start and the current moment.
func TestGexSessionGrid(t *testing.T) {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}

	// Saturday noon: window covers Fri Jul 31 + Thu Jul 30 (history start),
	// nothing older, no weekend slots.
	grid := gexSessionGrid(time.Date(2026, 8, 1, 12, 0, 0, 0, et))
	if len(grid) != 150 { // 2 sessions x 75 slots (09:50..16:00 step 5m)
		t.Fatalf("grid len = %d, want 150", len(grid))
	}
	first := time.Unix(grid[0], 0).In(et)
	last := time.Unix(grid[len(grid)-1], 0).In(et)
	if first.Format("Mon 2006-01-02 15:04") != "Thu 2026-07-30 09:50" {
		t.Errorf("first slot = %v", first)
	}
	if last.Format("Mon 2006-01-02 15:04") != "Fri 2026-07-31 16:00" {
		t.Errorf("last slot = %v", last)
	}
	for i := 1; i < len(grid); i++ {
		if grid[i] <= grid[i-1] {
			t.Fatalf("grid not ascending at %d", i)
		}
	}

	// Mid-session: the current partial day ends at the last elapsed slot.
	grid = gexSessionGrid(time.Date(2026, 7, 31, 12, 3, 0, 0, et))
	last = time.Unix(grid[len(grid)-1], 0).In(et)
	if last.Format("2006-01-02 15:04") != "2026-07-31 12:00" {
		t.Errorf("mid-session last slot = %v", last)
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
