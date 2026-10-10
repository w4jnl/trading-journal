package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// gexProfileJSONV070 is the engine v0.7.0 shape: expiry_filter echo (null
// when unfiltered), per-strike OI, and the futures denomination fields.
// Filtered responses carry the blend/index/etf slice only, decay null.
func gexProfileJSONV070(filter string) string {
	if filter != "" {
		return fmt.Sprintf(`{
			"market": "ES", "as_of": "2026-07-31T19:50:02.722581Z", "oi_date": "2026-07-31",
			"spot_index": 7494.55, "bucket_width": 10.0, "sources": ["SPX", "SPY"],
			"spot_futures": 7523.03, "futures_conversion": {"ratio": 1.0038, "source": "persisted", "paired_at": "2026-07-31T19:44:39Z", "age_seconds": 60},
			"expiry_filter": [%q],
			"profiles": {
				"blend": {"strikes_index": [7450, 7530], "net_gex": [1.5e9, 2.5e9], "call_gex": [2.0e9, 2.7e9], "put_gex": [-0.5e9, -0.2e9], "call_oi": [1500, 2600], "put_oi": [900, 300]},
				"index": {"strikes_index": [7450, 7530], "net_gex": [1.0e9, 2.0e9], "call_gex": [1.5e9, 2.2e9], "put_gex": [-0.5e9, -0.2e9], "call_oi": [150, 260], "put_oi": [90, 30]},
				"etf":   {"strikes_index": [7450, 7530], "net_gex": [0.5e9, 0.5e9], "call_gex": [0.5e9, 0.5e9], "put_gex": [0, 0], "call_oi": [1350, 2340], "put_oi": [810, 270]}
			},
			"expiry_decay": null
		}`, filter)
	}
	return `{
		"market": "ES", "as_of": "2026-07-31T19:50:02.722581Z", "oi_date": "2026-07-31",
		"spot_index": 7494.55, "bucket_width": 10.0, "sources": ["SPX", "SPY"],
		"spot_futures": 7523.03, "futures_conversion": {"ratio": 1.0038, "source": "persisted", "paired_at": "2026-07-31T19:44:39Z", "age_seconds": 60},
		"expiry_filter": null,
		"profiles": {
			"blend":    {"strikes_index": [7300, 7450, 7530], "net_gex": [-3.9e9, 6.1e9, 1.08e10], "call_gex": [1.0e9, 7.0e9, 1.2e10], "put_gex": [-4.9e9, -0.9e9, -1.2e9], "call_oi": [1000, 7000, 12000], "put_oi": [4900, 900, 1200]},
			"0dte":     {"strikes_index": [7450, 7530], "net_gex": [1.1e9, 2.1e9], "call_gex": [1.5e9, 2.3e9], "put_gex": [-0.4e9, -0.2e9], "call_oi": [500, 700], "put_oi": [300, 100]},
			"0dte_vol": {"strikes_index": [7450, 7530], "net_gex": [0.9e9, 1.9e9], "call_gex": [1.2e9, 2.0e9], "put_gex": [-0.3e9, -0.1e9], "call_oi": [500, 700], "put_oi": [300, 100]}
		},
		"expiry_decay": {"2026-08-01": -2.0e9, "2026-07-31": -6.0e9, "2026-08-21": 4.0e9}
	}`
}

// gexExpoEngineV070 fakes a v0.7.0 engine: ?expiry= is honored and echoed.
// The second return reads the last /v1/profile query the engine received.
func gexExpoEngineV070(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var lastQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/profile/") {
			mu.Lock()
			lastQ = r.URL.RawQuery
			mu.Unlock()
			doc := gexProfileJSONV070(r.URL.Query().Get("expiry"))
			// v0.8.0 ?bucket=: multiples of the stored width 10 up to 100
			// re-aggregate (the fake just relabels the width - enough for
			// contract tests); anything else 422s like the real engine.
			if b := r.URL.Query().Get("bucket"); b != "" {
				var n int
				if _, err := fmt.Sscanf(b, "%d", &n); err != nil || n < 10 || n > 100 || n%10 != 0 {
					http.Error(w, `{"detail":"bucket must be a multiple of this snapshot's stored width 10, at most 100"}`, http.StatusUnprocessableEntity)
					return
				}
				doc = strings.Replace(doc, `"bucket_width": 10.0`, fmt.Sprintf(`"bucket_width": %d.0`, n), 1)
			}
			fmt.Fprint(w, doc)
			return
		}
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02.722581Z"))
	}))
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return lastQ }
}

// gexExpoGet routes through a mux carrying ALL gex routes, proving the
// literal /gex/exposure patterns beat GET /gex/{market} (and /data beats
// /gex/exposure/{market}).
func gexExpoGet(t *testing.T, h *Handlers, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gex", h.GEXPage)
	mux.HandleFunc("GET /gex/exposure", h.GEXExposurePage)
	mux.HandleFunc("GET /gex/exposure/{market}", h.GEXExposurePage)
	mux.HandleFunc("GET /gex/exposure/data/{market}", h.GEXExposureData)
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
		// Engine v0.9.0 (update 007): 1Di band lines + flow-suffixed vol
		// magnitudes flow through the shared ladder builder.
		`"label":"1Di hi","v":7557.51,"kind":"em","family":"all"`,
		`+1.9B flow`,
		"not served by the engine yet",
		`data-prof="0dte_vol"`,
		">CALL<", "location.reload",
		// Time scrub: shared session grid + slider + moment-carry anchors
		// (rendered whenever history has slots, i.e. always in practice).
		`id="gxe-scrub"`,
		`id="gxe-scrub-when"`,
		`id="gxe-latest"`,
		`id="gxe-market-seg"`,
		`id="gx-tab-overview"`,
		"/static/js/gex-chips.js",
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

// Against a v0.7.0 engine the chips go live (data-chip-exp), the blob
// advertises the capability, and denomination comes from the payload itself.
func TestGEXExposurePageV070(t *testing.T) {
	db := gexTestDB(t)
	srv, _ := gexExpoEngineV070(t)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test"})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	rec := gexExpoGet(t, h, "/gex/exposure")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /gex/exposure = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-chip-exp="2026-08-21"`,
		`"supportsExpiry":true`,
		`"callOi":[1000,7000,12000]`,
		`"denom":"futures"`,
		`"ratio":1.0038`,
		`"indexName":"SPX"`,
		`id="gxe-denom-seg"`, // FUT <-> SPX display toggle
		`>SPX</button>`,
		`id="gxe-bucket-seg"`, // bucket width control: the ES ladder 5-10-25-50
		`"bucketOpts":[{"w":5,"ok":false},{"w":10,"ok":true},{"w":25,"ok":false},{"w":50,"ok":true}]`,
		`data-bw="10" class="on"`,        // native grid pre-selected
		`data-bw="50"`,                   // coarser multiple servable
		`<button type="button" disabled`, // 5/25 offered but grayed on a 10-pt snapshot
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	for _, not := range []string{"not served by the engine yet", `data-chip-prof=`} {
		if strings.Contains(body, not) {
			t.Errorf("body still contains legacy %q", not)
		}
	}

	// Market-switch links carry the moment + filter, including a
	// multi-select set (deduped, order preserved).
	rec = gexExpoGet(t, h, "/gex/exposure?at=2026-07-31T15:50&expiry=2026-08-21")
	if body := rec.Body.String(); !strings.Contains(body, `href="/gex/exposure/NQ?at=2026-07-31T15%3A50&amp;expiry=2026-08-21"`) {
		t.Errorf("QuerySuffix missing from market links")
	}
	rec = gexExpoGet(t, h, "/gex/exposure?expiry=2026-08-21,2026-08-24,2026-08-21")
	if body := rec.Body.String(); !strings.Contains(body, `href="/gex/exposure/NQ?expiry=2026-08-21%2C2026-08-24"`) {
		t.Errorf("multi-expiry QuerySuffix wrong")
	}
	// view= rides the QuerySuffix. Market seg links are EXPLICIT (an
	// explicit visit is a deliberate state change), while the TAB links
	// are the bare sticky paths - switching tabs must restore the stored
	// view, never wipe it by loading a filterless default.
	rec = gexExpoGet(t, h, "/gex/exposure?view=0dte")
	body = rec.Body.String()
	for _, want := range []string{
		`href="/gex/exposure/NQ?view=0dte"`,
		`href="/gex/exposure/ES?view=0dte"`,
		`href="/gex"`, // tabs partial: bare sticky Overview link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(body, `href="/gex/ES"`) {
		t.Errorf("tab link went explicit; away-and-back would wipe stored filters")
	}
	// Under ?at= the tabs go explicit market + at (historical browsing;
	// the colon is urlescaped by html/template).
	rec = gexExpoGet(t, h, "/gex/exposure/NQ?at=2026-07-31T15:50")
	if body := rec.Body.String(); !strings.Contains(body, `href="/gex/NQ?at=2026-07-31T15%3a50"`) {
		t.Errorf("at-mode tab link not explicit")
	}

	// Unix-seconds ?at= (the scrub's wire form) rides the QuerySuffix and
	// shows the Latest link; live pages render it hidden.
	rec = gexExpoGet(t, h, "/gex/exposure?at=1785503520&expiry=2026-08-21")
	body = rec.Body.String()
	if !strings.Contains(body, `href="/gex/exposure/NQ?at=1785503520&amp;expiry=2026-08-21"`) {
		t.Errorf("unix-at QuerySuffix missing from market links")
	}
	if !strings.Contains(body, `id="gxe-latest" href="/gex/exposure/ES">`) {
		t.Errorf("historical view should show the Latest link")
	}
	rec = gexExpoGet(t, h, "/gex/exposure")
	if !strings.Contains(rec.Body.String(), `id="gxe-latest" href="/gex/exposure/ES" style="display: none;"`) {
		t.Errorf("live view should hide the Latest link")
	}
}

// The data endpoint returns the filtered blob as JSON, futures-denominated
// from the payload alone (no snapshot call), and maps the engine's
// error contract onto useful statuses.
func TestGEXExposureData(t *testing.T) {
	db := gexTestDB(t)
	srv, srvQuery := gexExpoEngineV070(t)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test"})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	rec := gexExpoGet(t, h, "/gex/exposure/data/ES?expiry=2026-08-21")
	if rec.Code != http.StatusOK {
		t.Fatalf("data = %d: %s", rec.Code, rec.Body.String())
	}
	var blob struct {
		Denom    string `json:"denom"`
		Spot     float64
		NetTotal string `json:"netTotal"`
		Profiles map[string]struct {
			Strikes []float64 `json:"strikes"`
			CallOI  []float64 `json:"callOi"`
		} `json:"profiles"`
		Levels   []any `json:"levels"`
		Expiries []any `json:"expiries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &blob); err != nil {
		t.Fatal(err)
	}
	all, ok := blob.Profiles["all"]
	if !ok || len(blob.Profiles) != 1 {
		t.Fatalf("filtered profiles = %v", blob.Profiles)
	}
	if blob.Denom != "futures" || all.Strikes[0] != 7450*1.0038 {
		t.Errorf("denom/strike = %q / %v (payload futures fields must win without a snapshot)", blob.Denom, all.Strikes[0])
	}
	if len(all.CallOI) != 2 || blob.NetTotal != "+4.0B" {
		t.Errorf("callOi/netTotal = %v / %q", all.CallOI, blob.NetTotal)
	}
	if len(blob.Levels) != 0 || len(blob.Expiries) != 0 {
		t.Errorf("data blob should carry no levels/expiries (client keeps page-load ones)")
	}

	// Junk params are rejected before touching the engine.
	if rec := gexExpoGet(t, h, "/gex/exposure/data/CL?expiry=2026-08-21"); rec.Code != http.StatusNotFound {
		t.Errorf("junk market = %d", rec.Code)
	}
	// A malformed expiry 400s instead of silently serving the whole book -
	// including one bad member of an otherwise valid set.
	if rec := gexExpoGet(t, h, "/gex/exposure/data/ES?expiry=notadate"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed expiry = %d", rec.Code)
	}
	if rec := gexExpoGet(t, h, "/gex/exposure/data/ES?expiry=2026-08-21,zzz"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed set member = %d", rec.Code)
	}

	// Multi-select: the comma-joined set reaches the engine verbatim.
	rec = gexExpoGet(t, h, "/gex/exposure/data/ES?expiry=2026-08-21,2026-09-18")
	if rec.Code != http.StatusOK {
		t.Fatalf("multi = %d: %s", rec.Code, rec.Body.String())
	}
	if got := srvQuery(); got != "expiry=2026-08-21%2C2026-09-18" && got != "expiry=2026-08-21,2026-09-18" {
		t.Errorf("engine query = %q", got)
	}

	// Bucket re-aggregation: forwarded, and the blob reports the served
	// grid (bucketIndex raw, bucketWidth futures-scaled).
	rec = gexExpoGet(t, h, "/gex/exposure/data/ES?bucket=20")
	if rec.Code != http.StatusOK {
		t.Fatalf("bucket = %d: %s", rec.Code, rec.Body.String())
	}
	var bblob struct {
		BucketIndex float64 `json:"bucketIndex"`
		BucketWidth float64 `json:"bucketWidth"`
		Profiles    map[string]json.RawMessage
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bblob); err != nil {
		t.Fatal(err)
	}
	if bblob.BucketIndex != 20 || bblob.BucketWidth != 20*1.0038 || len(bblob.Profiles) != 3 {
		t.Errorf("bucket blob: idx=%v w=%v profiles=%d", bblob.BucketIndex, bblob.BucketWidth, len(bblob.Profiles))
	}
	if !strings.Contains(srvQuery(), "bucket=20") {
		t.Errorf("bucket not forwarded: %q", srvQuery())
	}
	// Composes with expiry; engine-invalid widths map to 422 with the
	// engine's detail; junk 400s before touching the engine.
	if rec := gexExpoGet(t, h, "/gex/exposure/data/ES?expiry=2026-08-21&bucket=20"); rec.Code != http.StatusOK {
		t.Errorf("expiry+bucket = %d", rec.Code)
	}
	if rec := gexExpoGet(t, h, "/gex/exposure/data/ES?bucket=7"); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "stored width") {
		t.Errorf("bucket=7 = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := gexExpoGet(t, h, "/gex/exposure/data/ES?bucket=abc"); rec.Code != http.StatusBadRequest {
		t.Errorf("bucket=abc = %d", rec.Code)
	}

	// A legacy engine silently ignores ?expiry= - the client sentinel must
	// surface as a 422, never as unfiltered-data-presented-as-filtered.
	legacy := gexExpoEngine(t, http.StatusOK)
	defer legacy.Close()
	lc, _ := gex.New(gex.Options{BaseURL: legacy.URL})
	lh := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: lc}
	rec = gexExpoGet(t, lh, "/gex/exposure/data/ES?expiry=2026-08-21")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "v0.7.0") {
		t.Errorf("legacy engine = %d: %s", rec.Code, rec.Body.String())
	}
	// A pre-v0.7.0 engine silently ignores ?bucket=: the served-width
	// mismatch surfaces as 422, never as mislabeled data.
	rec = gexExpoGet(t, lh, "/gex/exposure/data/ES?bucket=20")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("legacy bucket = %d: %s", rec.Code, rec.Body.String())
	}

	// The engine's per-expiry 404 becomes a friendly JSON 404.
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"no per-expiry data for this snapshot"}`, http.StatusNotFound)
	}))
	defer gone.Close()
	gc, _ := gex.New(gex.Options{BaseURL: gone.URL})
	gh := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: gc}
	rec = gexExpoGet(t, gh, "/gex/exposure/data/ES?expiry=2026-08-21")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unavailable for this moment") {
		t.Errorf("no-expiry-data = %d: %s", rec.Code, rec.Body.String())
	}

	// Malformed ?at= is a 400 here (programmatic endpoint), never a silent
	// live view under a historical UI.
	if rec := gexExpoGet(t, h, "/gex/exposure/data/ES?at=garbage"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed at = %d", rec.Code)
	}

	// A moment-addressed fetch (unix at, the scrub's wire form) is a FULL
	// moment: snapshot levels + regime ride along, chip stats come from the
	// unfiltered book even under a filter, and stale/today anchor to the
	// viewed moment - not the wall clock.
	atSec := time.Date(2026, 7, 31, 19, 52, 2, 0, time.UTC).Unix()
	rec = gexExpoGet(t, h, fmt.Sprintf("/gex/exposure/data/ES?at=%d&expiry=2026-08-21&fallback=1", atSec))
	if rec.Code != http.StatusOK {
		t.Fatalf("at slot = %d: %s", rec.Code, rec.Body.String())
	}
	var slot struct {
		Profiles map[string]json.RawMessage `json:"profiles"`
		Levels   []any                      `json:"levels"`
		Regime   string                     `json:"regime"`
		Expiries []struct {
			Date string `json:"date"`
			DTE  int    `json:"dte"`
		} `json:"expiries"`
		Stale    bool   `json:"stale"`
		Today    string `json:"today"`
		Degraded string `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &slot); err != nil {
		t.Fatal(err)
	}
	if len(slot.Profiles) != 1 || len(slot.Levels) == 0 || slot.Regime == "" {
		t.Errorf("slot blob: profiles=%d levels=%d regime=%q (want filtered slice + snapshot chrome)", len(slot.Profiles), len(slot.Levels), slot.Regime)
	}
	if len(slot.Expiries) != 3 || slot.Expiries[0].Date != "2026-07-31" || slot.Expiries[0].DTE != 0 {
		t.Errorf("slot chips = %+v (want the unfiltered book's strip, DTE anchored at the viewed day)", slot.Expiries)
	}
	if slot.Stale || slot.Today != "2026-07-31" || slot.Degraded != "" {
		t.Errorf("slot anchors: stale=%v today=%q degraded=%q", slot.Stale, slot.Today, slot.Degraded)
	}

	// fallback=1 + an unservable bucket degrades server-side: the request
	// paints the native grid and names what was dropped, instead of 422ing
	// (a scrub slot must always paint something).
	rec = gexExpoGet(t, h, fmt.Sprintf("/gex/exposure/data/ES?at=%d&bucket=7&fallback=1", atSec))
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback bucket = %d: %s", rec.Code, rec.Body.String())
	}
	var deg struct {
		Degraded    string  `json:"degraded"`
		BucketIndex float64 `json:"bucketIndex"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &deg); err != nil {
		t.Fatal(err)
	}
	if deg.Degraded != "bucket" || deg.BucketIndex != 10 {
		t.Errorf("degraded bucket: %+v", deg)
	}

	// fallback=1 + missing per-expiry data drops the filter instead: whole
	// book, degraded "expiry", selection preserved client-side.
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/profile/") {
			if r.URL.Query().Get("expiry") != "" {
				http.Error(w, `{"detail":"no per-expiry data for this snapshot"}`, http.StatusNotFound)
				return
			}
			fmt.Fprint(w, gexProfileJSONV070(""))
			return
		}
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02.722581Z"))
	}))
	defer fb.Close()
	fc, _ := gex.New(gex.Options{BaseURL: fb.URL})
	fh := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: fc}
	rec = gexExpoGet(t, fh, fmt.Sprintf("/gex/exposure/data/ES?at=%d&expiry=2026-08-21&fallback=1", atSec))
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback expiry = %d: %s", rec.Code, rec.Body.String())
	}
	var fdeg struct {
		Degraded string                     `json:"degraded"`
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fdeg); err != nil {
		t.Fatal(err)
	}
	if fdeg.Degraded != "expiry" || len(fdeg.Profiles) != 3 {
		t.Errorf("degraded expiry: %q, profiles=%d", fdeg.Degraded, len(fdeg.Profiles))
	}
	// Without fallback the same request keeps the honest 404.
	rec = gexExpoGet(t, fh, fmt.Sprintf("/gex/exposure/data/ES?at=%d&expiry=2026-08-21", atSec))
	if rec.Code != http.StatusNotFound {
		t.Errorf("no-fallback = %d, want 404", rec.Code)
	}
}

// Chip DTE math anchors to the VIEWED moment, so historical strips read as
// that day did (a scrubbed Sep-1 book shows its own 0d, not today's).
func TestGexBuildExpiryChips(t *testing.T) {
	tz := time.UTC
	decay := map[string]float64{"2026-08-21": 1e9, "2026-08-24": -1e9}
	chips := gexBuildExpiryChips(decay, time.Date(2026, 8, 21, 14, 0, 0, 0, tz), tz)
	if len(chips) != 2 || chips[0].DTE != 0 || chips[1].DTE != 3 {
		t.Fatalf("chips = %+v", chips)
	}
	chips = gexBuildExpiryChips(decay, time.Date(2026, 8, 24, 14, 0, 0, 0, tz), tz)
	if chips[0].DTE != -3 || chips[1].DTE != 0 {
		t.Errorf("historical anchor: %+v", chips)
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

// The per-market width ladder: servable = base or a coarser multiple within
// the engine's 100-point ceiling.
func TestGexBucketOpts(t *testing.T) {
	flat := func(opts []gexExpoBucketOpt) string {
		var b strings.Builder
		for _, o := range opts {
			fmt.Fprintf(&b, "%d:%v ", o.W, o.Ok)
		}
		return strings.TrimSpace(b.String())
	}
	for _, tc := range []struct {
		market string
		base   int
		want   string
	}{
		{"ES", 5, "5:true 10:true 25:true 50:true"},
		{"ES", 10, "5:false 10:true 25:false 50:true"}, // pre-v0.8.0 row stored at 10
		{"ES", 25, "5:false 10:false 25:true 50:true"},
		{"SPX", 5, "5:true 10:true 25:true 50:true"},
		{"NQ", 10, "10:true 20:true 50:true 100:true"}, // NDX strikes sit 10 apart: no 5, no 25
		{"NQ", 20, "10:false 20:true 50:false 100:true"},
		{"NDX", 10, "10:true 20:true 50:true 100:true"},
		{"ES", 20, "20:true 5:false 10:false 25:false 50:false"}, // odd base still offered
		{"RTY", 25, "25:true 50:true 125:false 250:false"},      // unknown market: base x1/2/5/10, capped at 100
	} {
		if got := flat(gexBucketOpts(tc.market, tc.base)); got != tc.want {
			t.Errorf("%s base %d: %q, want %q", tc.market, tc.base, got, tc.want)
		}
	}
	if gexBucketOpts("ES", 0) != nil {
		t.Errorf("base 0 should yield no options")
	}
	if got := gexCanonLadder("", 0); len(got) != 4 || got[0] != 5 {
		t.Errorf("fallback ladder = %v", got)
	}
}

// NQ is stored at 10 points, so its ladder is 10/20/50/100 - four servable
// widths instead of the two the ES ladder would leave it.
func TestGEXExposureNQLadder(t *testing.T) {
	db := gexTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/profile/") {
			// expiry_filter marks a v0.7.0+ engine, which is what shows the
			// bucket control at all.
			fmt.Fprint(w, strings.Replace(gexProfileJSONWith("2026-07-31T19:50:02.722581Z", `"expiry_filter": null`), `"market": "ES"`, `"market": "NQ"`, 1))
			return
		}
		fmt.Fprint(w, strings.Replace(gexSnapshotJSON("2026-07-31T19:50:02Z"), `"market": "ES"`, `"market": "NQ"`, 1))
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test", Timeout: 2 * time.Second})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	body := gexExpoGet(t, h, "/gex/exposure/NQ").Body.String()
	for _, want := range []string{
		`"bucketOpts":[{"w":10,"ok":true},{"w":20,"ok":true},{"w":50,"ok":true},{"w":100,"ok":true}]`,
		`data-bw="10" class="on"`, `data-bw="20"`, `data-bw="100"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("NQ exposure missing %q", want)
		}
	}
	if strings.Contains(body, `data-bw="5"`) || strings.Contains(body, `data-bw="25"`) {
		t.Error("NQ exposure offers ES widths")
	}
	// The overview's bin control gets the same ladder through the chart JSON.
	page := gexPageGet(t, h, "/gex/NQ").Body.String()
	if !strings.Contains(page, `"binLadder":[10,20,50,100]`) {
		t.Errorf("NQ overview chart JSON missing the NQ ladder")
	}
	// Same ES fixtures on ES: the ES ladder.
	srvES := gexExpoEngine(t, http.StatusOK)
	defer srvES.Close()
	cES, _ := gex.New(gex.Options{BaseURL: srvES.URL, APIKey: "test", Timeout: 2 * time.Second})
	h.GEX = cES
	if page := gexPageGet(t, h, "/gex").Body.String(); !strings.Contains(page, `"binLadder":[5,10,25,50]`) {
		t.Errorf("ES overview chart JSON missing the ES ladder")
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

	// Nil snapshot + pre-v0.7.0 payload: index denomination, no levels.
	e2 := gexBuildExpo(pd, nil, now, tz)
	if e2.Denom != "index" || len(e2.Levels) != 0 || e2.Spot != 7494.55 {
		t.Errorf("nil-snapshot degrade: denom=%q levels=%d spot=%v", e2.Denom, len(e2.Levels), e2.Spot)
	}
	if e2.SupportsExpiry || e2.Today != "2026-08-01" {
		t.Errorf("caps: supports=%v today=%q", e2.SupportsExpiry, e2.Today)
	}

	// v0.7.0 payload: its own futures fields win even with NO snapshot, OI
	// rides along when well-formed, and the capability flag surfaces.
	pdFut := 7494.55 * 1.004
	pd.SpotFutures = &pdFut
	pd.FuturesConversion = &gex.FuturesConversion{Ratio: 1.004}
	pd.SupportsExpiry = true
	blend := pd.Profiles["blend"]
	blend.CallOI = []float64{100, 200}
	blend.PutOI = []float64{300, 400}
	pd.Profiles["blend"] = blend
	e3 := gexBuildExpo(pd, nil, now, tz)
	if e3.Denom != "futures" || e3.Spot != pdFut || e3.Profiles["all"].Strikes[0] != 7300*1.004 {
		t.Errorf("payload denomination: denom=%q spot=%v strike=%v", e3.Denom, e3.Spot, e3.Profiles["all"].Strikes[0])
	}
	// The blob exposes the ratio (index display mode divides by it) only
	// when futures-denominated; e2 above is index-denominated.
	if e3.Ratio != 1.004 || e2.Ratio != 0 || e3.IndexName != "index" {
		t.Errorf("ratio/indexName: %v / %v / %q", e3.Ratio, e2.Ratio, e3.IndexName)
	}
	if !e3.SupportsExpiry || e3.Profiles["all"].CallOI[1] != 200 || e3.Profiles["all"].PutOI[0] != 300 {
		t.Errorf("caps/OI: %v %v %v", e3.SupportsExpiry, e3.Profiles["all"].CallOI, e3.Profiles["all"].PutOI)
	}
	// Mismatched OI lengths are dropped, never rendered off-by-one.
	blend.PutOI = []float64{300}
	pd.Profiles["blend"] = blend
	if e4 := gexBuildExpo(pd, nil, now, tz); e4.Profiles["all"].CallOI != nil {
		t.Errorf("malformed OI should be dropped")
	}
	// The payload ratio also beats a DIFFERENT snapshot ratio.
	e5 := gexBuildExpo(pd, s, now, tz)
	if e5.Spot != pdFut || e5.BucketWidth != 10*1.004 {
		t.Errorf("payload-vs-snapshot precedence: spot=%v bw=%v", e5.Spot, e5.BucketWidth)
	}
	if len(e5.Levels) == 0 {
		t.Errorf("levels must still flow from the snapshot")
	}
}
