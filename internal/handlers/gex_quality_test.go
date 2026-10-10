package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Engine v0.11.0 (update 009) fixtures: the data_quality / provenance /
// pc_volume_legs objects spliced into the shared snapshot payload.
const (
	gexDegradedDQ = `"data_quality": {"degraded": true, "warnings": [
		{"code": "STALE_LEG", "severity": "degraded", "symbol": "SPY", "message": "SPY leg 18 min older than SPX"},
		{"code": "STALE_PAYLOAD", "severity": "notice", "symbol": null, "message": "payload age 900s"}],
		"funnel": {"raw": 12000}, "xcheck": {"median_rel_err": 0.034}, "rate_source": "fred:SOFR",
		"snapshot_age_seconds": 912, "quote_effective_at": "2026-07-31T19:35:02+00:00"}`
	gexCleanDQ    = `"data_quality": {"degraded": false, "warnings": [], "rate_source": "fred:SOFR", "snapshot_age_seconds": 61, "quote_effective_at": "2026-07-31T19:35:02+00:00"}`
	gexReconProv  = `"provenance": {"kind": "reconstructed", "chain_as_of": "2026-07-31T18:50:02.101Z", "spot_source": "mw_marks", "spot_as_of": "2026-07-31T19:50:00Z", "reason": "vendor outage 19:00-20:00Z"}`
	gexMeasProv   = `"provenance": {"kind": "measured", "chain_as_of": "2026-07-31T19:50:01Z", "spot_source": "mw_marks", "spot_as_of": "2026-07-31T19:50:02Z", "reason": null}`
	gexLegsJSON   = `"pc_volume_legs": {"SPY": {"put": 5000000, "call": 4400000, "ratio": 1.1364}, "SPX": {"put": 1321745, "call": 1137231, "ratio": 1.1622}}`
	gexDegradedAt = "2026-07-31T19:50:02.722581Z"
)

// gexSnapshotJSONWith appends extra top-level members to gexSnapshotJSON.
func gexSnapshotJSONWith(asOf string, extra ...string) string {
	return gexSpliceJSON(gexSnapshotJSON(asOf), extra)
}

// gexProfileJSONWith does the same for the profile payload.
func gexProfileJSONWith(asOf string, extra ...string) string {
	return gexSpliceJSON(gexProfileJSON(asOf), extra)
}

func gexSpliceJSON(base string, extra []string) string {
	base = strings.TrimSpace(base)
	if len(extra) == 0 {
		return base
	}
	return base[:len(base)-1] + ",\n" + strings.Join(extra, ",\n") + "\n}"
}

// gexSteppingEngine serves a degraded row at gexDegradedAt for any ?at= at
// or after it, and a clean row five minutes earlier for anything before -
// the shape CleanSnapshot walks.
func gexSteppingEngine(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	degradedAsOf, _ := time.Parse(time.RFC3339Nano, gexDegradedAt)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		at, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("at"))
		if err != nil {
			http.Error(w, `{"detail":"bad at"}`, http.StatusUnprocessableEntity)
			return
		}
		if !at.Before(degradedAsOf) {
			fmt.Fprint(w, gexSnapshotJSONWith(gexDegradedAt, gexDegradedDQ, gexMeasProv))
			return
		}
		fmt.Fprint(w, gexSnapshotJSONWith("2026-07-31T19:45:02.1Z", gexCleanDQ, gexMeasProv))
	}))
}

func TestGexAnnotateStepsPastDegradedRow(t *testing.T) {
	db := gexTestDB(t)
	var calls atomic.Int64
	srv := gexSteppingEngine(t, &calls)
	defer srv.Close()
	h := gexTestHandlers(t, db, srv.URL)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	gexSeedEntry(t, db, 1, aiPlaceholderBody)
	(&gexAnnotator{h: h}).annotate(ctx, 1, gexTestTrade("MES"))

	// Both moments (15:52 / 15:58 ET) hit the degraded 15:50 row first and
	// step to the clean 15:45 one: two calls per moment.
	if calls.Load() != 4 {
		t.Errorf("engine calls = %d, want 4 (degraded + step, twice)", calls.Load())
	}
	body := gexEntryBody(t, db, 1)
	for _, want := range []string{"| entry 15:52 ET | exit 15:58 ET |", " quality=ok", " provenance=measured"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"(degraded)", "context degraded", "stale=true"} {
		if strings.Contains(body, bad) {
			t.Errorf("body should not carry %q after stepping to a clean row:\n%s", bad, body)
		}
	}
}

func TestGexAnnotateKeepsDegradedRowWhenNoCleanOne(t *testing.T) {
	db := gexTestDB(t)
	var calls atomic.Int64
	// Every ?at= answers with the same degraded row: the walk finds nothing
	// better and settles for it, flagged.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, gexSnapshotJSONWith(gexDegradedAt, gexDegradedDQ))
	}))
	defer srv.Close()
	h := gexTestHandlers(t, db, srv.URL)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	gexSeedEntry(t, db, 1, aiPlaceholderBody)
	(&gexAnnotator{h: h}).annotate(ctx, 1, gexTestTrade("MES"))

	// 1 + gexCleanSteps calls per moment, then it gives up.
	if want := int64(2 * (1 + gexCleanSteps)); calls.Load() != want {
		t.Errorf("engine calls = %d, want %d", calls.Load(), want)
	}
	body := gexEntryBody(t, db, 1)
	for _, want := range []string{
		"| entry 15:52 ET (degraded) | exit 15:58 ET (degraded) |",
		"_entry context degraded: STALE_LEG SPY - SPY leg 18 min older than SPX_",
		"_exit context degraded: STALE_LEG SPY - SPY leg 18 min older than SPX_",
		" quality=degraded provenance=measured em_next=null -->", // null provenance reads as measured
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// The notice-severity finding is not what degraded the row; the
	// footnote lists degraded findings only.
	if strings.Contains(body, "STALE_PAYLOAD") {
		t.Errorf("notice finding leaked into the degraded footnote:\n%s", body)
	}
}

func TestGexAnnotateReconstructedRow(t *testing.T) {
	db := gexTestDB(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, gexSnapshotJSONWith(gexDegradedAt, gexCleanDQ, gexReconProv))
	}))
	defer srv.Close()
	h := gexTestHandlers(t, db, srv.URL)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	gexSeedEntry(t, db, 1, aiPlaceholderBody)
	(&gexAnnotator{h: h}).annotate(ctx, 1, gexTestTrade("MES"))

	if calls.Load() != 2 {
		t.Errorf("engine calls = %d, want 2 (a clean row is never stepped past)", calls.Load())
	}
	body := gexEntryBody(t, db, 1)
	for _, want := range []string{
		"| entry 15:52 ET (reconstructed) | exit 15:58 ET (reconstructed) |",
		"_entry context reconstructed after a feed outage: the Jul 31 14:50 ET chain repriced at this moment's spot — walls near-exact, flip/regime carry that chain's IVs_",
		" quality=ok provenance=reconstructed em_next=null -->",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// A refresh cannot improve a clean reconstructed block (the history is
	// immutable), so the page must not offer one.
	if gexRefreshable(body) {
		t.Error("clean reconstructed block reported as refreshable")
	}
}

// gexQualityPageEngine serves quality-bearing snapshot + profile payloads
// and a configurable /health body.
func gexQualityPageEngine(t *testing.T, snapExtra []string, health string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health":
			fmt.Fprint(w, health)
		case strings.HasPrefix(r.URL.Path, "/v1/profile/"):
			fmt.Fprint(w, gexProfileJSONWith("2026-07-31T19:50:02.722581Z", snapExtra...))
		default:
			fmt.Fprint(w, gexSnapshotJSONWith("2026-07-31T19:50:02.722581Z", snapExtra...))
		}
	}))
}

func TestGEXPageQualityStrip(t *testing.T) {
	db := gexTestDB(t)
	srv := gexQualityPageEngine(t, []string{gexDegradedDQ, gexReconProv, gexLegsJSON}, `{"status":"ok","reasons":[],"notices":[]}`)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test", Timeout: 2 * time.Second})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	rec := gexPageGet(t, h, "/gex")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /gex = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<div id="gx-quality" class="gex-quality mb-16">`, // shown (no hidden attribute)
		`>degraded</span>`,
		`>reconstructed</span>`,
		`STALE_LEG · SPY</span> <span class="muted">SPY leg 18 min older than SPX</span>`,
		`<span class="pill warn">STALE_PAYLOAD</span> <span class="muted">payload age 900s</span>`,
		`reconstructed after a feed outage: the Jul 31 14:50 ET chain repriced`,
		`id="gx-pclegs"`, `>SPX 1.16 · SPY 1.14</div>`, // per-leg ratios, index leg first
		`"pcLegs":"SPX 1.16 · SPY 1.14"`,
		`"quality":{"show":true,"degraded":true,"kind":"reconstructed"`,
		`"class":"error"`, `"class":"warn"`,
		`/static/js/gex-quality.js`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// A healthy engine adds nothing to the session-closed banner.
	if strings.Contains(body, "Engine reports") {
		t.Errorf("healthy engine must not add notes to the banner")
	}

	// Pre-v0.11.0 row: nothing to say, the strip is hidden and the JSON
	// says so.
	srv2 := gexQualityPageEngine(t, nil, `{"status":"ok"}`)
	defer srv2.Close()
	c2, _ := gex.New(gex.Options{BaseURL: srv2.URL, APIKey: "test", Timeout: 2 * time.Second})
	h.GEX = c2
	body = gexPageGet(t, h, "/gex").Body.String()
	if !strings.Contains(body, `<div id="gx-quality" class="gex-quality mb-16" hidden>`) {
		t.Errorf("clean row: strip should be hidden")
	}
	if !strings.Contains(body, `"quality":{"show":false,"degraded":false}`) {
		t.Errorf("clean row: quality JSON = %s", regexpFind(body, `"quality":\{[^}]*\}`))
	}
	if !strings.Contains(body, `<div class="sub" id="gx-pclegs" title="Put/call ratio per blend leg — index and ETF contracts counted apart (engine v0.11.0)"></div>`) {
		t.Errorf("clean row: legs line should be empty")
	}
}

func TestGEXPageEngineNotesOnStaleLiveRow(t *testing.T) {
	db := gexTestDB(t)
	// The live row is from 2026-07-31 while "now" is later: stale live
	// view, so the page asks /health and relays the engine's own reasons.
	srv := gexQualityPageEngine(t, nil,
		`{"status":"degraded","reasons":["vendor host unreachable since 13:52Z", {"code":"NO_CYCLE","message":"no successful cycle this session"}],"notices":["marks stale"]}`)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test", Timeout: 2 * time.Second})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	body := gexPageGet(t, h, "/gex").Body.String()
	want := "Engine reports: status degraded; vendor host unreachable since 13:52Z; no successful cycle this session; marks stale."
	if !strings.Contains(body, want) {
		t.Errorf("banner missing %q in:\n%s", want, regexpFind(body, `(?s)<div class="flash info mb-16" role="status">.*?</div>`))
	}
	// A historical view (?at=) is expected to lag its moment; no /health call
	// and no notes.
	body = gexPageGet(t, h, "/gex?at=2026-07-31T16:30").Body.String()
	if strings.Contains(body, "Engine reports") {
		t.Errorf("historical view must not carry engine notes")
	}
}

func TestGEXExposureQualityStrip(t *testing.T) {
	db := gexTestDB(t)
	srv := gexQualityPageEngine(t, []string{gexDegradedDQ, gexReconProv}, `{"status":"ok"}`)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test", Timeout: 2 * time.Second})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	rec := gexExpoGet(t, h, "/gex/exposure/ES")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /gex/exposure/ES = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<div id="gxe-quality" class="gex-quality" style="margin-bottom: 6px;">`,
		`>degraded</span>`, `>reconstructed</span>`, `STALE_LEG · SPY</span>`,
		`"quality":{"show":true,"degraded":true,"kind":"reconstructed"`,
		`/static/js/gex-quality.js`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposure page missing %q", want)
		}
	}
	// The scrub/data endpoint carries the same verdict per moment.
	rec = gexExpoGet(t, h, "/gex/exposure/data/ES?at=1785527402")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"quality":{"show":true,"degraded":true`) {
		t.Errorf("data endpoint: code %d body %s", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}
}

func TestGexAnalysisFactsQuality(t *testing.T) {
	em := 9.7
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
	clean := gexAnalysisFacts(s, "ES")
	for _, bad := range []string{"DATA QUALITY", "PROVENANCE", "provenance:", "data quality"} {
		if strings.Contains(clean, bad) {
			t.Errorf("pre-v0.11.0 snapshot must not emit %q (retained reads stay byte-identical):\n%s", bad, clean)
		}
	}

	chain := time.Date(2026, 7, 31, 18, 50, 2, 0, time.UTC)
	s.DataQuality = &gex.DataQuality{Degraded: true, Warnings: []gex.QualityWarning{
		{Code: "STALE_LEG", Severity: "degraded", Symbol: "SPY", Message: "SPY leg 18 min older than SPX"},
		{Code: "STALE_PAYLOAD", Severity: "notice", Message: "payload age 900s"},
	}}
	s.Provenance = &gex.Provenance{Kind: "reconstructed", ChainAsOf: gex.ISOTime{Time: chain}}
	facts := gexAnalysisFacts(s, "ES")
	for _, want := range []string{
		"DATA QUALITY: DEGRADED - STALE_LEG SPY - SPY leg 18 min older than SPX. Treat the regime percentile, the 1h/1d changes and any flip timing as unreliable",
		"PROVENANCE: RECONSTRUCTED - this row was rebuilt after a feed outage from the option chain as of 14:50 ET repriced at this moment's spot",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts missing %q:\n%s", want, facts)
		}
	}
	if strings.Contains(facts, "STALE_PAYLOAD") {
		t.Errorf("degraded row: the notice is noise next to the degraded finding:\n%s", facts)
	}

	// Notices without a degraded verdict are worth one quiet line; a
	// recomputed row is as good as measured and says so.
	s.DataQuality.Degraded = false
	s.DataQuality.Warnings = s.DataQuality.Warnings[1:]
	s.Provenance = &gex.Provenance{Kind: "recomputed"}
	facts = gexAnalysisFacts(s, "ES")
	for _, want := range []string{
		"data quality notices (not degrading): STALE_PAYLOAD - payload age 900s",
		"provenance: recomputed from this slot's archived payloads - as trustworthy as a live measurement",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts missing %q:\n%s", want, facts)
		}
	}
	if strings.Contains(facts, "DATA QUALITY: DEGRADED") {
		t.Errorf("clean row flagged degraded:\n%s", facts)
	}
}

func TestGexPCLegsLine(t *testing.T) {
	if got := gexPCLegsLine(nil); got != "" {
		t.Errorf("nil legs = %q", got)
	}
	legs := map[string]gex.PCVolume{
		"SPY": {Put: 5e6, Call: 4.4e6, Ratio: 1.1364},
		"SPX": {Put: 1.3e6, Call: 1.1e6, Ratio: 1.1622},
		"XSP": {Put: 10, Call: 0, Ratio: 0}, // no call volume: skipped, never "0.00"
	}
	if got := gexPCLegsLine(legs); got != "SPX 1.16 · SPY 1.14" {
		t.Errorf("legs line = %q", got)
	}
}

func TestReplaceGexBlock(t *testing.T) {
	oldBlock := "_Gamma context (GEX) — futures prices_\n\n| a |\n\n<!-- tj-gex:v1 stale=true -->"
	newBlock := "_Gamma context (GEX) — futures prices_\n\n| b |\n\n<!-- tj-gex:v1 stale=false quality=ok provenance=reconstructed -->"
	body := "My notes.\n\n---\n\n_Note added 2026-09-28 17:00 CEST_\n\n" + oldBlock + "\n\nMore notes after.\n"

	out, found, changed := replaceGexBlock(body, newBlock)
	if !found || !changed {
		t.Fatalf("found=%v changed=%v", found, changed)
	}
	want := "My notes.\n\n---\n\n_Note added 2026-09-28 17:00 CEST_\n\n" + newBlock + "\n\nMore notes after.\n"
	if out != want {
		t.Errorf("replaced body:\n%s\nwant:\n%s", out, want)
	}
	if _, _, changed := replaceGexBlock(out, newBlock); changed {
		t.Error("identical block reported as changed")
	}
	if _, found, _ := replaceGexBlock("no gamma here", newBlock); found {
		t.Error("block found in a body without one")
	}
	// A marker without its heading (hand-edited body) is not a block we can
	// swap; the caller appends instead of corrupting text.
	if _, found, _ := replaceGexBlock("text <!-- tj-gex:v1 x=1 --> tail", newBlock); found {
		t.Error("orphan marker treated as a block")
	}

	// Refreshable: stale or degraded blocks only.
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"<!-- tj-gex:v1 stale=true quality=null provenance=measured -->", true},
		{"<!-- tj-gex:v1 stale=false quality=degraded provenance=measured -->", true},
		{"<!-- tj-gex:v1 stale=false quality=ok provenance=reconstructed -->", false},
		{"<!-- tj-gex:v1 stale=false -->", false}, // older block, clean
		{"no marker stale=true", false},
	} {
		if got := gexRefreshable(tc.body); got != tc.want {
			t.Errorf("gexRefreshable(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// gexRefreshDB is the refresh tests' fixture: gexTestDB already carries the
// account/trade/users the refresh handler joins (user 7's closed trade 42).
func gexRefreshDB(t *testing.T) *sql.DB {
	t.Helper()
	return gexTestDB(t)
}

func gexRefreshPost(t *testing.T, h *Handlers, entryID any) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /journal/{id}/gex/refresh", h.JournalGexRefresh)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester"})
	req := httptest.NewRequest("POST", fmt.Sprintf("/journal/%v/gex/refresh", entryID), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestJournalGexRefresh(t *testing.T) {
	db := gexRefreshDB(t)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	// Annotate during the "outage": the only row is the prior session's
	// close, so both moments come back stale.
	var staleCalls atomic.Int64
	stale := gexFakeEngine("2026-07-30T20:00:02Z", &staleCalls)
	defer stale.Close()
	h := gexTestHandlers(t, db, stale.URL)
	h.Templates = web.TemplatesFS
	gexSeedEntry(t, db, 1, aiPlaceholderBody)
	if _, err := db.Exec(`UPDATE journal_entries SET trade_id = 42 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	(&gexAnnotator{h: h}).annotate(ctx, 1, gexTestTrade("MES"))
	before := gexEntryBody(t, db, 1)
	if !strings.Contains(before, "stale=true") || !gexRefreshable(before) {
		t.Fatalf("setup: expected a stale, refreshable block:\n%s", before)
	}

	// Group entry with its own text, linked to the same trade.
	if _, err := db.Exec(`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (2, 7, 'Thesis text.', ?, 'group')`,
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO journal_entry_trades VALUES (2, 42)`); err != nil {
		t.Fatal(err)
	}
	(&gexAnnotator{h: h}).annotate(ctx, 2, gexTestTrade("MES"))
	groupBefore := gexEntryBody(t, db, 2)
	if !strings.HasPrefix(groupBefore, "Thesis text.") || !strings.Contains(groupBefore, "_Note added ") {
		t.Fatalf("setup: group body unexpected:\n%s", groupBefore)
	}

	// The engine has since backfilled the outage with reconstructed rows.
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, gexSnapshotJSONWith(gexDegradedAt, gexCleanDQ, gexReconProv))
	}))
	defer fresh.Close()
	c, _ := gex.New(gex.Options{BaseURL: fresh.URL, APIKey: "test", Timeout: 2 * time.Second})
	h.GEX = c

	rec := gexRefreshPost(t, h, 1)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/journal/trade/42?flash=gex-refreshed" {
		t.Fatalf("trade refresh: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	after := gexEntryBody(t, db, 1)
	for _, want := range []string{
		"---\n\n_Gamma context (GEX)", // the seeded frame survived, block replaced in place
		"(reconstructed)", "quality=ok provenance=reconstructed", "stale=false",
	} {
		if !strings.Contains(after, want) {
			t.Errorf("refreshed body missing %q:\n%s", want, after)
		}
	}
	if strings.Contains(after, "stale=true") || strings.Count(after, gexMarker) != 1 {
		t.Errorf("refreshed body should hold exactly one fresh block:\n%s", after)
	}
	// Same rows again: nothing to write.
	if rec := gexRefreshPost(t, h, 1); rec.Header().Get("Location") != "/journal/trade/42?flash=gex-unchanged" {
		t.Errorf("second refresh: %s", rec.Header().Get("Location"))
	}
	if gexEntryBody(t, db, 1) != after {
		t.Error("unchanged refresh rewrote the body")
	}

	// Group: the user's own text and the note frame stay, the block swaps.
	if rec := gexRefreshPost(t, h, 2); rec.Header().Get("Location") != "/journal/group/2?flash=gex-refreshed" {
		t.Errorf("group refresh: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	groupAfter := gexEntryBody(t, db, 2)
	if !strings.HasPrefix(groupAfter, "Thesis text.") || !strings.Contains(groupAfter, "_Note added ") ||
		!strings.Contains(groupAfter, "provenance=reconstructed") || strings.Contains(groupAfter, "stale=true") {
		t.Errorf("group body after refresh:\n%s", groupAfter)
	}

	// Scope and ownership guards.
	if _, err := db.Exec(`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (3, 7, 'day', ?, 'day'), (4, 8, 'theirs', ?, 'trade')`,
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if rec := gexRefreshPost(t, h, 3); rec.Code != http.StatusBadRequest {
		t.Errorf("day scope: %d, want 400", rec.Code)
	}
	if rec := gexRefreshPost(t, h, 4); rec.Code != http.StatusNotFound {
		t.Errorf("other user's entry: %d, want 404", rec.Code)
	}
	if rec := gexRefreshPost(t, h, 999); rec.Code != http.StatusNotFound {
		t.Errorf("missing entry: %d, want 404", rec.Code)
	}
	if rec := gexRefreshPost(t, h, "abc"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id: %d, want 400", rec.Code)
	}

	// Engine down: flash, body untouched.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	down.Close()
	c2, _ := gex.New(gex.Options{BaseURL: down.URL})
	h.GEX = c2
	if rec := gexRefreshPost(t, h, 1); rec.Header().Get("Location") != "/journal/trade/42?flash=gex-failed" {
		t.Errorf("engine down: %s", rec.Header().Get("Location"))
	}
	if gexEntryBody(t, db, 1) != after {
		t.Error("failed refresh changed the body")
	}
}

// regexpFind returns the first match of pattern in s, for focused failure
// output on multi-thousand-line page bodies.
func regexpFind(s, pattern string) string {
	return regexp.MustCompile(pattern).FindString(s)
}
