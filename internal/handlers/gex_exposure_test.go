package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// gexProfileJSON is a /v1/profile fixture shaped like the live engine
// (abridged): blend/0dte/0dte_vol with call/put splits, three decay dates.
func gexProfileJSON(asOf string) string {
	return fmt.Sprintf(`{
		"market": "ES", "as_of": %q, "oi_date": "2026-07-31",
		"spot_index": 7494.55, "bucket_width": 10.0, "sources": ["SPX", "SPY"],
		"profiles": {
			"blend":    {"strikes_index": [7300, 7450, 7530], "net_gex": [-3.9e9, 6.1e9, 1.08e10], "call_gex": [1.0e9, 7.0e9, 1.2e10], "put_gex": [-4.9e9, -0.9e9, -1.2e9]},
			"index":    {"strikes_index": [7300, 7450, 7530], "net_gex": [-3.0e9, 5.0e9, 9.0e9], "call_gex": [1.0e9, 6.0e9, 1.0e10], "put_gex": [-4.0e9, -1.0e9, -1.0e9]},
			"0dte":     {"strikes_index": [7450, 7530], "net_gex": [1.1e9, 2.1e9], "call_gex": [1.5e9, 2.3e9], "put_gex": [-0.4e9, -0.2e9]},
			"0dte_vol": {"strikes_index": [7450, 7530], "net_gex": [0.9e9, 1.9e9], "call_gex": [1.2e9, 2.0e9], "put_gex": [-0.3e9, -0.1e9]}
		},
		"expiry_decay": {"2026-08-01": -2.0e9, "2026-07-31": -6.0e9, "2026-08-21": 4.0e9}
	}`, asOf)
}

// gexExpoEngine fakes both engine endpoints, dispatching on path.
func gexExpoEngine(t *testing.T, profileStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/profile/") {
			if profileStatus != http.StatusOK {
				http.Error(w, `{"detail":"nope"}`, profileStatus)
				return
			}
			fmt.Fprint(w, gexProfileJSON("2026-07-31T19:50:02.722581Z"))
			return
		}
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02.722581Z"))
	}))
}

// gexExpoGet routes through a mux carrying ALL gex routes, proving the
// literal /gex/exposure patterns beat GET /gex/{market}.
func gexExpoGet(t *testing.T, h *Handlers, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gex", h.GEXPage)
	mux.HandleFunc("GET /gex/exposure", h.GEXExposurePage)
	mux.HandleFunc("GET /gex/exposure/{market}", h.GEXExposurePage)
	mux.HandleFunc("GET /gex/{market}", h.GEXPage)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester", DisplayName: "Tester"})
	req := httptest.NewRequest("GET", target, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestGEXExposurePage(t *testing.T) {
	db := gexTestDB(t)
	srv := gexExpoEngine(t, http.StatusOK)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test"})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	rec := gexExpoGet(t, h, "/gex/exposure")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /gex/exposure = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		">Overview<", ">Exposure<", // tabs
		"const gexExpoInit",
		`"denom":"futures"`,
		`"strikes":[7327.74`, // 7300 x 1.0038: same futures scaling as the overview
		"7558.58",            // published futures CW, never re-derived
		`"family":"0dte_vol"`,
		`"0dte":`, // profile key present in blob
		"Jul 31",  // expiry chip label
		"needs engine support",
		`data-prof="0dte_vol"`,
		">CALL<", "location.reload",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Tabs mark Exposure active on this page.
	if !strings.Contains(body, `class="active">Exposure`) {
		t.Errorf("Exposure tab not active")
	}

	// NQ routes through; junk market 404s.
	if rec := gexExpoGet(t, h, "/gex/exposure/NQ"); rec.Code != http.StatusOK {
		t.Errorf("NQ = %d", rec.Code)
	}
	if rec := gexExpoGet(t, h, "/gex/exposure/CL"); rec.Code != http.StatusNotFound {
		t.Errorf("CL = %d, want 404", rec.Code)
	}
}

// An engine without /v1/profile degrades to an explanatory empty state with
// the tabs intact - the Overview keeps working, so navigation must survive.
func TestGEXExposureNoProfile(t *testing.T) {
	db := gexTestDB(t)
	srv := gexExpoEngine(t, http.StatusNotFound)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	rec := gexExpoGet(t, h, "/gex/exposure")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "doesn't serve GET /v1/profile") {
		t.Errorf("code %d, empty state missing", rec.Code)
	}
	if !strings.Contains(body, ">Overview<") {
		t.Errorf("tabs missing from degraded page")
	}
	if strings.Contains(body, "const gexExpoInit") {
		t.Errorf("blob rendered despite missing profile")
	}
}

// gexBuildExpo is pure: exercise scaling, families, expiry math directly.
func TestGexBuildExpo(t *testing.T) {
	pd := &gex.ProfileDetail{
		AsOf: time.Date(2026, 7, 31, 19, 50, 2, 0, time.UTC), OIDate: "2026-07-31",
		SpotIndex: 7494.55, BucketWidth: 10,
		Profiles: map[string]gex.ProfileSet{
			"blend": {StrikesIndex: []float64{7300, 7530}, NetGex: []float64{-3.9e9, 1.08e10}, CallGex: []float64{1e9, 1.2e10}, PutGex: []float64{-4.9e9, -1.2e9}},
			"0dte":  {StrikesIndex: []float64{7450}, NetGex: []float64{1e9}, CallGex: []float64{1.2e9}, PutGex: []float64{-0.2e9}},
		},
		ExpiryDecay: map[string]float64{"2026-08-03": 1e9, "2026-08-01": -3e9},
	}
	ratio := 1.0038
	fut := 7494.55 * ratio
	cwFut := 7558.58
	s := &gex.Snapshot{
		SpotIndex: 7494.55, SpotFutures: &fut,
		FuturesConversion: &gex.FuturesConversion{Ratio: ratio},
		Levels: []gex.Level{
			{Type: "call_wall", Bucket: "all", Index: 7530, Futures: &cwFut, Gex: 1.08e10},
		},
	}
	// Fixed "now" in a non-UTC trading TZ: Aug 1 local.
	tz, _ := time.LoadLocation("America/Chicago")
	now := time.Date(2026, 8, 1, 9, 30, 0, 0, tz)

	e := gexBuildExpo(pd, s, now, tz)
	if e.Denom != "futures" {
		t.Fatalf("denom = %q", e.Denom)
	}
	if got := e.Profiles["all"].Strikes[0]; got != 7300*ratio {
		t.Errorf("scaled strike = %v", got)
	}
	if e.BucketWidth != 10*ratio {
		t.Errorf("bucketWidth = %v", e.BucketWidth)
	}
	if _, ok := e.Profiles["0dte"]; !ok {
		t.Errorf("0dte profile missing")
	}
	var cw *gexExpoLevel
	for i := range e.Levels {
		if e.Levels[i].Label == "CW" {
			cw = &e.Levels[i]
		}
	}
	if cw == nil || cw.V != cwFut {
		t.Errorf("CW level = %+v, want published futures %v", cw, cwFut)
	}
	// Expiries sorted ascending, pct = |net|/sum|net|, DTE from tz midnights.
	if len(e.Expiries) != 2 || e.Expiries[0].Date != "2026-08-01" || e.Expiries[1].Date != "2026-08-03" {
		t.Fatalf("expiry order = %+v", e.Expiries)
	}
	if e.Expiries[0].Pct != "75%" || e.Expiries[1].Pct != "25%" {
		t.Errorf("pct = %s / %s", e.Expiries[0].Pct, e.Expiries[1].Pct)
	}
	if e.Expiries[0].DTE != 0 || e.Expiries[1].DTE != 2 {
		t.Errorf("dte = %d / %d", e.Expiries[0].DTE, e.Expiries[1].DTE)
	}
	if !e.Expiries[0].Neg || e.Expiries[1].Neg {
		t.Errorf("neg flags = %v / %v", e.Expiries[0].Neg, e.Expiries[1].Neg)
	}

	// Nil snapshot: index denomination, no levels.
	e2 := gexBuildExpo(pd, nil, now, tz)
	if e2.Denom != "index" || len(e2.Levels) != 0 || e2.Spot != 7494.55 {
		t.Errorf("nil-snapshot degrade: denom=%q levels=%d spot=%v", e2.Denom, len(e2.Levels), e2.Spot)
	}
}
