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
		"positive",                    // regime tile
		"&#43;74.5B",                  // net gex tile (html/template escapes the plus)
		"7465.79",                     // flip futures
		"Call wall",                   // levels table
		`const gexData`,               // chart blob
		`"strikes":[7327.74,7558.614`, // futures-scaled strike axis (index x 1.0038)
		"ET",                          // as-of label
		// Engine v0.5.0 surface (update 002):
		"1h -9.2B",                    // net GEX change sub-line on the tile
		"Gamma wall",                  // 0DTE wall row label
		"Gamma magnet 1",              // ranked magnet row label
		"Prior EM low",                // prior-session EM band row label
		`<td class="muted">0dte</td>`, // 0DTE bucket column
		`"flip0":7488.35`,             // ladder: 0DTE flip
		`"gw0":7528.5`,                // ladder: 0DTE gamma wall
		`"em1High":7548.29`,           // ladder: prior EM band (futures denom)
		`"magnets":[{"v":7528.5,"n":1,"gex":"+8.4B"}`, // ladder: G1 leads the magnet list
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The bucket guard end-to-end: the fixture lists the 0DTE call wall
	// (7538.54) FIRST, but the ladder/chart wall stays the all-book one.
	if !strings.Contains(body, `"cw":7558.58`) {
		t.Errorf("ladder cw is not the all-book wall")
	}
	// The prior-day EM band's contractual gex 0.0 renders as absent, not as a
	// measured "+0.0B".
	if strings.Contains(body, "0.0B") {
		t.Errorf("EM band gex rendered as a measured zero")
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
	// The picker's minute is queried at its END, so a grid minute resolves to
	// that minute's snapshot rather than the previous slot.
	if lastAt != "2026-07-31T10:54:59Z" {
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
// futures-vs-futures, and the profile chart is denominated like the ladder
// (futures when the basis is warm) so the two price axes read as one. A
// reviewer reading one unlabeled flip number next to an index-led spot tile
// previously mistook this for a unit bug - the numbers were right, the
// labeling was not.
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
		"index 7437.55",        // flip's index value (tile sub-line)
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

	// The chart is denominated like the ladder - futures when the basis is
	// warm - so its two price axes never disagree with the panel beside it.
	// Reference lines use the engine's PUBLISHED futures values (flip
	// 7465.788..., not index 7437.55 x ratio = 7465.82), and the strike axis
	// is index x the snapshot's stored ratio (7300 x 1.0038 = 7327.74).
	i := strings.Index(body, "const gexInit")
	blob := body[i : i+strings.Index(body[i:], "\n")]
	for _, want := range []string{
		`"denom":"futures"`,
		`"flip":7465.788539891167`, // published futures value, verbatim
		`"strikes":[7327.74,`,      // scaled strike axis
	} {
		if !strings.Contains(blob, want) {
			t.Errorf("chart blob missing %q: %s", want, blob)
		}
	}
	if strings.Contains(blob, `"flip":7437.55`) {
		t.Errorf("chart flip still index-denominated")
	}
}

// The regime ladder resolves to ONE denomination: futures while the basis is
// warm, index when it goes cold, never a silent mix. The expected-move band
// is spot +/- EM, with EM scaled into futures terms the same way the engine
// scales its own levels.
func TestGexBuildLadder(t *testing.T) {
	ratio := 1.0038
	spot := 7494.55
	em := 9.7
	cwFut, pwFut, flipFut := 7558.58, 7327.71, 7465.79
	snap := &gex.Snapshot{
		SpotIndex:         spot,
		FuturesConversion: &gex.FuturesConversion{Ratio: ratio},
		Regime:            gex.Regime{State: "positive"},
		Flip:              gex.Flip{Index: f64(7437.55), Futures: &flipFut},
		Levels: []gex.Level{
			{Type: "call_wall", Index: 7530, Futures: &cwFut, Gex: 1.08e10, Bucket: "all"},
			{Type: "put_wall", Index: 7300, Futures: &pwFut, Gex: -3.9e9, Bucket: "all"},
		},
		ExpectedMove1D: &em,
	}

	spotFut := spot * ratio
	lad := gexBuildLadder(snap, &spotFut)
	if lad.Denom != "futures" {
		t.Fatalf("denom = %q, want futures", lad.Denom)
	}
	if *lad.CW != cwFut || *lad.PW != pwFut || *lad.Flip != flipFut {
		t.Errorf("levels not futures-denominated: %+v", lad)
	}
	// EM is index-denominated at source; the futures band scales it.
	if got, want := *lad.EMHigh-lad.Spot, em*ratio; got-want > 1e-9 || want-got > 1e-9 {
		t.Errorf("EM half-width = %v, want %v (em scaled by ratio)", got, want)
	}
	if *lad.EMLow >= lad.Spot || *lad.EMHigh <= lad.Spot {
		t.Errorf("EM band does not straddle spot: %+v", lad)
	}
	if lad.CWGex != "+10.8B" || lad.PWGex != "-3.9B" {
		t.Errorf("wall gex = %q / %q", lad.CWGex, lad.PWGex)
	}

	// Cold basis: the whole ladder falls back to index rather than emptying.
	snap.FuturesConversion = nil
	lad = gexBuildLadder(snap, nil)
	if lad.Denom != "index" || lad.Spot != spot {
		t.Fatalf("cold basis denom/spot = %q/%v", lad.Denom, lad.Spot)
	}
	if *lad.CW != 7530 || *lad.Flip != 7437.55 {
		t.Errorf("cold basis levels not index: %+v", lad)
	}
	if got, want := *lad.EMHigh-lad.Spot, em; got-want > 1e-9 || want-got > 1e-9 {
		t.Errorf("cold-basis EM half-width = %v, want unscaled %v", got, want)
	}

	// No flip (no zero crossing that day) must not drop the other levels.
	snap.Flip = gex.Flip{}
	if lad = gexBuildLadder(snap, nil); lad.Flip != nil || lad.CW == nil {
		t.Errorf("null flip mishandled: %+v", lad)
	}
}

func f64(v float64) *float64 { return &v }

// The engine publishes spot_futures from v0.3.0 and the contract asks
// consumers never to re-derive a futures price. The published value must win
// outright; the ratio arithmetic survives only as a fallback for an older
// engine, and neither applies when the basis is cold.
func TestGexSpotFutures(t *testing.T) {
	published := 7532.25
	s := &gex.Snapshot{
		SpotIndex:         7503.54,
		SpotFutures:       &published,
		FuturesConversion: &gex.FuturesConversion{Ratio: 1.5}, // absurd on purpose
	}
	if got := gexSpotFutures(s); got == nil || *got != published {
		t.Errorf("= %v, want the published %v (not ratio arithmetic)", got, published)
	}

	s.SpotFutures = nil
	s.FuturesConversion = &gex.FuturesConversion{Ratio: 1.004}
	if got := gexSpotFutures(s); got == nil || *got != 7503.54*1.004 {
		t.Errorf("pre-v0.3.0 fallback = %v", got)
	}

	s.FuturesConversion = nil
	if got := gexSpotFutures(s); got != nil {
		t.Errorf("cold basis = %v, want nil (never invent a conversion)", got)
	}
}

// Cold basis (engine v0.3.0 fixture shape): every futures field is null while
// index values, regime, profile and expected move stay populated. The page
// must degrade to index-only - not to "no data" - and say so.
func TestGEXPageColdBasis(t *testing.T) {
	db := gexTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"market": "ES", "as_of": "2026-07-31T20:00:03.058037Z",
			"spot_index": 7503.54, "spot_futures": null, "futures_conversion": null,
			"regime": {"state": "positive", "net_gex_at_spot": 44964082924.8574, "percentile_20d": null},
			"flip": {"index": 7442.540346680423, "futures": null},
			"levels": [
				{"type": "call_wall", "index": 7550.0, "futures": null, "gex": 5406021999.0, "bucket": "all"},
				{"type": "put_wall", "index": 7300.0, "futures": null, "gex": -3793769564.0, "bucket": "all"}
			],
			"profile": {"strikes_index": [6760.0, 6770.0], "net_gex": [-33493001.0, -131439900.0]},
			"expected_move_1d": 41.7
		}`)
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	rec := gexPageGet(t, h, "/gex")
	if rec.Code != http.StatusOK {
		t.Fatalf("cold basis = %d, want a rendered page", rec.Code)
	}
	body := rec.Body.String()
	// Index-denominated content survives; the ladder switches denomination.
	for _, want := range []string{
		"7503.54",                   // spot index
		"7442.54",                   // flip index, in tile + ladder row
		"positive",                  // regime
		"41.7 pts",                  // expected move
		`"denom":"index"`,           // ladder falls back rather than emptying
		"showing index values only", // and the page says why
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cold-basis page missing %q", want)
		}
	}
	// No fabricated futures price anywhere near spot or the flip.
	for _, forbidden := range []string{"7532.25", "7471.02", "7578.89"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("cold basis invented a futures price (%s)", forbidden)
		}
	}

	// The ladder must carry the raw, unscaled expected move in index terms.
	i := strings.Index(body, "const gexInit")
	blob := body[i : i+strings.Index(body[i:], "\n")]
	if !strings.Contains(blob, `"emHigh":7545.24`) {
		t.Errorf("cold-basis EM band not index-denominated: %s", blob)
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
