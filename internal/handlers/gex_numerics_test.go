package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/gexview"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Engine v0.12.x (update 010) fixtures.
const (
	gexEMNext      = `"expected_move_next_session": 37.7`
	gexSkewRR      = `"skew": {"dte0_pct": -6.1, "m1_pct": 28.7, "dte0_rr_pts": -0.019, "m1_rr_pts": 0.034}`
	gexRecompProv  = `"provenance": {"kind": "recomputed", "chain_as_of": "2026-07-31T19:50:01Z", "spot_source": "delayed_payload", "spot_as_of": null, "reason": "v0.12.0 numerics", "engine_version": "0.12.1", "settings_hash": "9f3a", "replaced": [{"as_of": "2026-07-31T19:50:02Z"}], "carried_degraded": []}`
	gexRecompDQ    = `"data_quality": {"degraded": false, "warnings": [{"code": "RECOMPUTED", "severity": "notice", "symbol": null, "message": "recomputed from archived cycle payloads"}], "straddle": {"expiry": "2026-07-31", "root": "SPXW", "strike": 7495, "market_mid": 9.7, "model_mid": 9.74, "rel_err": 0.0041, "t_hours": 0.1, "at_floor": false}}`
	gexNumericsSrc = `"skew": {"dte0_pct": -6.1, "m1_pct": 29.0}`
)

// gexSnapshotJSONNumerics is the shared snapshot with update-010 fields; the
// base fixture's skew object is swapped for one carrying the RR points.
func gexSnapshotJSONNumerics(asOf string, extra ...string) string {
	base := strings.Replace(gexSnapshotJSON(asOf), gexNumericsSrc, gexSkewRR, 1)
	return gexSpliceJSON(base, extra)
}

func TestSnapshotNumericsDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, gexSnapshotJSONNumerics(gexDegradedAt, gexEMNext, gexRecompProv, gexRecompDQ))
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL})
	s, err := c.Snapshot(context.Background(), "ES", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.ExpectedMoveNextSession == nil || *s.ExpectedMoveNextSession != 37.7 || *s.ExpectedMove1D != 9.7 {
		t.Errorf("expected moves = %v / %v", s.ExpectedMove1D, s.ExpectedMoveNextSession)
	}
	if s.Skew == nil || s.Skew.DTE0RRPts == nil || *s.Skew.DTE0RRPts != -0.019 || *s.Skew.M1RRPts != 0.034 || *s.Skew.M1Pct != 28.7 {
		t.Errorf("skew = %+v", s.Skew)
	}
	p := s.Provenance
	if p == nil || p.Kind != "recomputed" || p.Reason != "v0.12.0 numerics" || p.EngineVersion != "0.12.1" || p.SettingsHash != "9f3a" {
		t.Errorf("provenance = %+v", p)
	}
	// replaced[] / carried_degraded[] are not decoded and must not break the row.
	st := s.DataQuality.Straddle
	if st == nil || st.RelErr == nil || *st.RelErr != 0.0041 || st.Root != "SPXW" || st.Strike == nil || *st.Strike != 7495 {
		t.Fatalf("straddle = %+v", st)
	}
	if got := st.Label(); got != "straddle +0.4%" {
		t.Errorf("straddle label = %q", got)
	}
	var none *gex.Straddle
	if none.Label() != "" || (&gex.Straddle{}).Label() != "" {
		t.Error("straddle without rel_err must label as empty")
	}
}

func TestGexProvAndSkewLines(t *testing.T) {
	if got := gexview.ProvLine(nil, nil); got != "" {
		t.Errorf("empty prov line = %q", got)
	}
	rel := 0.0041
	dq := &gex.DataQuality{Straddle: &gex.Straddle{RelErr: &rel}}
	prov := &gex.Provenance{Kind: "recomputed", Reason: "v0.12.0 numerics"}
	if got := gexview.ProvLine(prov, dq); got != "recomputed: v0.12.0 numerics · straddle +0.4%" {
		t.Errorf("prov line = %q", got)
	}
	if got := gexview.ProvLine(&gex.Provenance{Kind: "recomputed"}, nil); got != "recomputed" {
		t.Errorf("reasonless recompute = %q", got)
	}
	// Measured and reconstructed rows say nothing here (the strip owns
	// reconstructed); the straddle still shows.
	if got := gexview.ProvLine(&gex.Provenance{Kind: "reconstructed"}, dq); got != "straddle +0.4%" {
		t.Errorf("reconstructed prov line = %q", got)
	}

	if got := gexview.SkewRRLine(nil); got != "" {
		t.Errorf("nil skew = %q", got)
	}
	d0, m1 := -0.019, 0.034
	if got := gexview.SkewRRLine(&gex.Skew{DTE0RRPts: &d0, M1RRPts: &m1}); got != "RR 0DTE -1.9 · 1M +3.4 vol pts" {
		t.Errorf("rr line = %q", got)
	}
	if got := gexview.SkewRRLine(&gex.Skew{M1RRPts: &m1}); got != "RR 1M +3.4 vol pts" {
		t.Errorf("rr line (one tenor) = %q", got)
	}
	if got := gexview.SkewRRLine(&gex.Skew{}); got != "" {
		t.Errorf("rr line without points = %q", got)
	}
}

// The RECOMPUTED notice every repaired row carries is provenance, not a
// trust finding: it must not light up the strip.
func TestGexQualityHidesRecomputedNotice(t *testing.T) {
	dq := &gex.DataQuality{Warnings: []gex.QualityWarning{{Code: "RECOMPUTED", Severity: "notice", Message: "recomputed from archived cycle payloads"}}}
	q := gexview.BuildQuality(dq, &gex.Provenance{Kind: "recomputed", Reason: "v0.12.0 numerics"})
	if q.Show || len(q.Warnings) != 0 || q.Kind != "" {
		t.Errorf("recomputed-only row lit the strip: %+v", q)
	}
	dq.Warnings = append(dq.Warnings, gex.QualityWarning{Code: "STALE_PAYLOAD", Severity: "notice", Message: "payload age 900s"})
	q = gexview.BuildQuality(dq, nil)
	if !q.Show || len(q.Warnings) != 1 || q.Warnings[0].Code != "STALE_PAYLOAD" {
		t.Errorf("real notice dropped with the RECOMPUTED one: %+v", q)
	}
}

func TestGEXPageNumerics(t *testing.T) {
	db := gexTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case strings.HasPrefix(r.URL.Path, "/v1/profile/"):
			fmt.Fprint(w, gexProfileJSONWith("2026-07-31T19:50:02.722581Z", gexRecompProv, gexRecompDQ))
		default:
			fmt.Fprint(w, gexSnapshotJSONNumerics("2026-07-31T19:50:02.722581Z", gexEMNext, gexRecompProv, gexRecompDQ))
		}
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test", Timeout: 2 * time.Second})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	body := gexPageGet(t, h, "/gex").Body.String()
	for _, want := range []string{
		`Expected move · <span id="gx-emlabel">next session</span>`,
		`id="gx-em">37.7 pts</div>`,
		`id="gx-empct">0.50% of spot · to close 9.7 pts</div>`,
		`id="gx-skewrr" title="The same risk reversal unnormalized: put IV − call IV in vol points (engine v0.12.0)">RR 0DTE -1.9 · 1M &#43;3.4 vol pts</div>`, // html/template escapes +
		`· snapshot Fri Jul 31, 15:50:02 ET · recomputed: v0.12.0 numerics · straddle &#43;0.4%</div>`,
		`"emLabel":"next session"`, `"skewRR":"RR 0DTE -1.9 · 1M +3.4 vol pts"`,
		`"provLine":"recomputed: v0.12.0 numerics · straddle +0.4%"`,
		// The RECOMPUTED notice stays out of the strip: hidden, nothing to say.
		`<div id="gx-quality" class="gex-quality mb-16" hidden>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, ">RECOMPUTED</span>") {
		t.Error("RECOMPUTED notice rendered as a strip pill")
	}

	// Exposure footer carries the same provenance suffix.
	expo := gexExpoGet(t, h, "/gex/exposure/ES").Body.String()
	if !strings.Contains(expo, `<span id="gxe-prov"> · recomputed: v0.12.0 numerics · straddle &#43;0.4%</span>`) ||
		!strings.Contains(expo, `"provLine":"recomputed: v0.12.0 numerics · straddle +0.4%"`) {
		t.Errorf("exposure footer missing the provenance suffix")
	}

	// Pre-v0.12.0 row: the 1d figure under the old label, no RR line, no suffix.
	srv2 := gexQualityPageEngine(t, nil, `{"status":"ok"}`)
	defer srv2.Close()
	c2, _ := gex.New(gex.Options{BaseURL: srv2.URL, APIKey: "test", Timeout: 2 * time.Second})
	h.GEX = c2
	body = gexPageGet(t, h, "/gex").Body.String()
	for _, want := range []string{
		`Expected move · <span id="gx-emlabel">1d</span>`,
		`id="gx-em">9.7 pts</div>`,
		`id="gx-empct">0.13% of spot</div>`,
		`· snapshot Fri Jul 31, 15:50:02 ET</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("old-row page missing %q", want)
		}
	}
	if strings.Contains(body, "vol pts") || strings.Contains(body, "to close") {
		t.Error("old row rendered v0.12.0-only lines")
	}
}

func TestGexAnalysisFactsNumerics(t *testing.T) {
	em, next := 9.7, 37.7
	flipFut := 7465.79
	s := &gex.Snapshot{
		AsOf:              time.Date(2026, 7, 31, 19, 50, 2, 0, time.UTC),
		SpotIndex:         7494.55,
		SpotFutures:       f64(7523.03),
		FuturesConversion: &gex.FuturesConversion{Ratio: 1.0038},
		Regime:            gex.Regime{State: "positive", NetGexTotal: f64(7.4e10)},
		Flip:              gex.Flip{Index: f64(7437.55), Futures: &flipFut},
		ExpectedMove1D:    &em,
	}
	old := gexAnalysisFacts(s, "ES")
	if !strings.Contains(old, "expected move (1 session, 1 standard deviation): 9.7 points") || strings.Contains(old, "next full session") {
		t.Errorf("pre-v0.12.0 facts changed:\n%s", old)
	}
	s.ExpectedMoveNextSession = &next
	s.Provenance = &gex.Provenance{Kind: "recomputed", Reason: "v0.12.0 numerics"}
	facts := gexAnalysisFacts(s, "ES")
	for _, want := range []string{
		"expected move to this session's close (1 standard deviation; the front expiry's straddle, which shrinks through the day): 9.7 points",
		"expected-move band to the close: 7513.29 to 7532.77",
		"expected move for the next full session (1 standard deviation): 37.8 points", // futures-scaled like every other distance
		"every 'x the expected move' multiple below uses the move to the close",
		"provenance: recomputed from this slot's archived payloads (v0.12.0 numerics) - as trustworthy as a live measurement",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts missing %q:\n%s", want, facts)
		}
	}
	// The yardstick stays the move to the close: 57.24 pts above the flip is
	// 5.9x of 9.74 (scaled), not 1.5x of the next-session figure.
	if !strings.Contains(facts, "5.9x the expected move") {
		t.Errorf("distance multiples must use the to-close move:\n%s", facts)
	}
}

func TestGexAnnotateEmNext(t *testing.T) {
	db := gexTestDB(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, gexSnapshotJSONNumerics(gexDegradedAt, gexEMNext, gexRecompProv, gexRecompDQ))
	}))
	defer srv.Close()
	h := gexTestHandlers(t, db, srv.URL)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})
	gexSeedEntry(t, db, 1, aiPlaceholderBody)
	(&gexAnnotator{h: h}).Annotate(ctx, 1, gexTestTrade("MES"))
	body := gexEntryBody(t, db, 1)
	if !strings.Contains(body, " em=9.7 stale=false quality=ok provenance=recomputed em_next=37.7 -->") {
		t.Errorf("marker keys:\n%s", body)
	}
	// A repaired row is not a trust finding: no flags in the header, no footnote.
	if strings.Contains(body, "(recomputed)") || strings.Contains(body, "context degraded") || strings.Contains(body, "RECOMPUTED") {
		t.Errorf("recomputed row flagged in the block:\n%s", body)
	}
}

func gexAdminPost(t *testing.T, h *Handlers, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/market-data/gex-refresh", h.AdminGexRefresh)
	mux.HandleFunc("GET /admin/market-data", h.AdminMarketDataPage)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester", IsAdmin: true})
	req := httptest.NewRequest("POST", "/admin/market-data/gex-refresh", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAdminGexRefreshBulk(t *testing.T) {
	db := gexRefreshDB(t)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	// Three in-window entries (2026-07-31) and one outside, all annotated
	// against the pre-repair engine; one more in-window entry without a
	// gamma block.
	var calls atomic.Int64
	before := gexFakeEngine("2026-07-31T19:50:02.722581Z", &calls)
	defer before.Close()
	h := gexTestHandlers(t, db, before.URL)
	h.Templates = web.TemplatesFS
	for _, id := range []int64{1, 2, 3} {
		gexSeedEntry(t, db, id, aiPlaceholderBody)
		if _, err := db.Exec(`UPDATE journal_entries SET trade_id = 42 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		(&gexAnnotator{h: h}).Annotate(ctx, id, gexTestTrade("MES"))
	}
	if _, err := db.Exec(`INSERT INTO journal_entries (id, user_id, body, entry_date, scope, trade_id) VALUES (4, 7, ?, ?, 'trade', 42), (5, 7, 'no gamma here', ?, 'trade', NULL)`,
		aiPlaceholderBody, time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	(&gexAnnotator{h: h}).Annotate(ctx, 4, gexTestTrade("MES"))
	outsideBefore := gexEntryBody(t, db, 4)

	// The engine repaired the rows: same as_of, corrected numbers (here: the
	// new next-session field and the recomputed provenance).
	after := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, gexSnapshotJSONNumerics("2026-07-31T19:50:02.722581Z", gexEMNext, gexRecompProv, gexRecompDQ))
	}))
	defer after.Close()
	c, _ := gex.New(gex.Options{BaseURL: after.URL, APIKey: "test", Timeout: 2 * time.Second})
	h.GEX = c

	rec := gexAdminPost(t, h, url.Values{"from": {"2026-07-31"}, "to": {"2026-07-31"}})
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || loc != "/admin/market-data?changed=3&failed=0&flash=gexrefresh&from=2026-07-31&same=0&to=2026-07-31&total=3" {
		t.Fatalf("bulk refresh: %d %s", rec.Code, loc)
	}
	for _, id := range []int64{1, 2, 3} {
		if b := gexEntryBody(t, db, id); !strings.Contains(b, "em_next=37.7") || strings.Count(b, gexMarker) != 1 {
			t.Errorf("entry %d not rewritten in place:\n%s", id, b)
		}
	}
	if gexEntryBody(t, db, 4) != outsideBefore {
		t.Error("entry outside the window was touched")
	}
	if b := gexEntryBody(t, db, 5); b != "no gamma here" {
		t.Errorf("entry without a block was touched: %q", b)
	}

	// Second pass: nothing changes.
	rec = gexAdminPost(t, h, url.Values{"from": {"2026-07-31"}, "to": {"2026-07-31"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "changed=0&failed=0") || !strings.Contains(loc, "same=3") {
		t.Errorf("second pass: %s", loc)
	}
	// Swapped bounds are tolerated; the whole range covers entry 4 too.
	rec = gexAdminPost(t, h, url.Values{"from": {"2026-08-01"}, "to": {"2026-07-01"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "from=2026-07-01") || !strings.Contains(loc, "to=2026-08-01") || !strings.Contains(loc, "total=4") {
		t.Errorf("swapped bounds: %s", loc)
	}
	if !strings.Contains(gexEntryBody(t, db, 4), "em_next=37.7") {
		t.Error("entry 4 not refreshed by the wide window")
	}

	// Bad dates, no engine, engine down (three failures in a row abort).
	if loc := gexAdminPost(t, h, url.Values{"from": {"yesterday"}}).Header().Get("Location"); loc != "/admin/market-data?flash=gexbad" {
		t.Errorf("bad date: %s", loc)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	down.Close()
	c2, _ := gex.New(gex.Options{BaseURL: down.URL})
	h.GEX = c2
	rec = gexAdminPost(t, h, url.Values{"from": {"2026-07-31"}, "to": {"2026-07-31"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "aborted=1") || !strings.Contains(loc, "failed=3") {
		t.Errorf("engine down: %s", loc)
	}
	h.GEX = nil
	if loc := gexAdminPost(t, h, nil).Header().Get("Location"); loc != "/admin/market-data?flash=gexoff" {
		t.Errorf("no engine: %s", loc)
	}

	// The market-data page renders the card and the last run's counts.
	h.GEX = c
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/market-data", h.AdminMarketDataPage)
	req := httptest.NewRequest("GET", "/admin/market-data?changed=3&failed=0&flash=gexrefresh&from=2026-07-31&same=0&to=2026-07-31&total=3", nil).
		WithContext(auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester", DisplayName: "Tester", IsAdmin: true}))
	pr := httptest.NewRecorder()
	mux.ServeHTTP(pr, req)
	page := pr.Body.String()
	for _, want := range []string{
		`action="/admin/market-data/gex-refresh"`,
		`name="from" value="2026-07-31"`,
		`3 entries with a gamma block — 3 rewritten, 0 unchanged, 0 failed.`,
		"Refresh gamma context",
	} {
		if pr.Code != http.StatusOK || !strings.Contains(page, want) {
			t.Errorf("admin page (%d) missing %q", pr.Code, want)
		}
	}
	// Default window starts at the repair date.
	req = httptest.NewRequest("GET", "/admin/market-data", nil).WithContext(auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester", DisplayName: "Tester", IsAdmin: true}))
	pr = httptest.NewRecorder()
	mux.ServeHTTP(pr, req)
	if !strings.Contains(pr.Body.String(), `name="from" value="`+gexRepairStart+`"`) {
		t.Error("default from should be the repair start date")
	}
}
