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

// stubAI returns a canned completion and counts calls, standing in for the
// configured local model.
type stubAI struct {
	calls  atomic.Int64
	out    string
	err    error
	system string
	user   string
}

func (s *stubAI) Generate(_ context.Context, system, user string) (string, error) {
	s.calls.Add(1)
	s.system, s.user = system, user
	return s.out, s.err
}

func gexAnalyzePost(t *testing.T, h *Handlers, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /gex/analyze/{market}", h.GEXAnalyze)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester", DisplayName: "Tester"})
	req := httptest.NewRequest("POST", target, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// A read is generated once per SNAPSHOT and then retained: a second request -
// even one arriving at a different moment that resolves to the same snapshot -
// must reuse the stored text without calling the model again.
func TestGEXAnalyzeRetainsPerSnapshot(t *testing.T) {
	db := gexTestDB(t)

	var engineCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engineCalls.Add(1)
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02Z"))
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL})
	ai := &stubAI{out: "  The read.\n\n- **Above the flip:** fades.  "}
	h := &Handlers{
		DB: db, Queries: models.New(db), Templates: web.TemplatesFS,
		Timezone: time.UTC, GEX: c, AI: ai, AIModel: "test-model",
	}

	rec := gexAnalyzePost(t, h, "/gex/analyze/ES?at=2026-07-31T19:50:02.5Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze = %d: %s", rec.Code, rec.Body.String())
	}
	if ai.calls.Load() != 1 {
		t.Fatalf("model calls = %d, want 1", ai.calls.Load())
	}
	if body := rec.Body.String(); !strings.Contains(body, "The read.") || !strings.Contains(body, "<strong>Above the flip:</strong>") {
		t.Errorf("fragment did not render the read as markdown: %s", body)
	}

	// A different requested moment resolving to the same snapshot: retained.
	rec = gexAnalyzePost(t, h, "/gex/analyze/ES?at=2026-07-31T19:53:00Z")
	if ai.calls.Load() != 1 {
		t.Errorf("retained read re-called the model (%d calls)", ai.calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "The read.") {
		t.Errorf("retained read not returned: %s", rec.Body.String())
	}
	// Stored exactly once, keyed by the snapshot's own as_of.
	var n int
	var storedKey, storedModel string
	if err := db.QueryRow(`SELECT count(*), max(as_of), max(model) FROM gex_analyses`).Scan(&n, &storedKey, &storedModel); err != nil {
		t.Fatal(err)
	}
	if n != 1 || storedKey != "2026-07-31T19:50:02Z" || storedModel != "test-model" {
		t.Errorf("stored rows=%d key=%q model=%q", n, storedKey, storedModel)
	}

	// force=1 regenerates and replaces in place.
	ai.out = "Second read."
	rec = gexAnalyzePost(t, h, "/gex/analyze/ES?at=2026-07-31T19:50:02.5Z&force=1")
	if ai.calls.Load() != 2 {
		t.Errorf("force did not regenerate (%d calls)", ai.calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "Second read.") {
		t.Errorf("force did not return the new read")
	}
	if err := db.QueryRow(`SELECT count(*) FROM gex_analyses`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("force added a row instead of replacing (%d rows)", n)
	}

	// The retained read rides along on the page and on the scrub payload.
	page := gexPageGet(t, h, "/gex").Body.String()
	if !strings.Contains(page, "Second read.") || !strings.Contains(page, "Analyze with AI") {
		t.Errorf("page did not render the retained read + button")
	}
	if !strings.Contains(page, `"analysisHTML":`) {
		t.Errorf("scrub payload missing analysisHTML")
	}
}

// Failure paths must not store anything or 500 the page: no snapshot, a dead
// engine, a model error, and the feature being switched off.
func TestGEXAnalyzeFailures(t *testing.T) {
	db := gexTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, gexSnapshotJSON("2026-07-31T19:50:02Z"))
	}))
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL})

	// AI disabled: route is invisible even though gex is on.
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}
	if rec := gexAnalyzePost(t, h, "/gex/analyze/ES?at=2026-07-31T19:50:02.5Z"); rec.Code != http.StatusNotFound {
		t.Errorf("ai disabled = %d, want 404", rec.Code)
	}
	page := gexPageGet(t, h, "/gex").Body.String()
	if strings.Contains(page, "Analyze with AI") {
		t.Errorf("button rendered with AI disabled")
	}

	failing := &stubAI{err: fmt.Errorf("model offline")}
	h.AI = failing
	if rec := gexAnalyzePost(t, h, "/gex/analyze/ES?at=notatime"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad at = %d, want 400", rec.Code)
	}
	if rec := gexAnalyzePost(t, h, "/gex/analyze/CL?at=2026-07-31T19:50:02.5Z"); rec.Code != http.StatusNotFound {
		t.Errorf("bad market = %d, want 404", rec.Code)
	}
	if rec := gexAnalyzePost(t, h, "/gex/analyze/ES?at=2026-07-31T19:50:02.5Z"); rec.Code != http.StatusBadGateway {
		t.Errorf("model error = %d, want 502", rec.Code)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM gex_analyses`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a failed read was stored (%d rows)", n)
	}

	// Engine down.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	down.Close()
	h.GEX, _ = gex.New(gex.Options{BaseURL: down.URL})
	h.AI = &stubAI{out: "x"}
	if rec := gexAnalyzePost(t, h, "/gex/analyze/ES?at=2026-07-31T19:50:02.5Z"); rec.Code != http.StatusBadGateway {
		t.Errorf("engine down = %d, want 502", rec.Code)
	}
}

// The facts block is what the model reasons over, so it must carry the
// geometry - not just the raw levels.
func TestGexAnalysisFacts(t *testing.T) {
	em := 9.7
	cwFut, pwFut, flipFut := 7558.58, 7327.71, 7465.79
	s := &gex.Snapshot{
		AsOf:              time.Date(2026, 7, 31, 19, 50, 2, 0, time.UTC),
		SpotIndex:         7494.55,
		SpotFutures:       f64(7523.03),
		FuturesConversion: &gex.FuturesConversion{Ratio: 1.0038},
		Regime:            gex.Regime{State: "positive", NetGexTotal: f64(7.4e10)},
		Flip:              gex.Flip{Index: f64(7437.55), Futures: &flipFut},
		Levels: []gex.Level{
			{Type: "call_wall", Index: 7530, Futures: &cwFut, Gex: 1.08e10, Bucket: "all"},
			{Type: "put_wall", Index: 7300, Futures: &pwFut, Gex: -3.9e9, Bucket: "all"},
		},
		Profile: gex.Profile{
			StrikesIndex: []float64{7300, 7350, 7400, 7437, 7500, 7530},
			NetGex:       []float64{-3.9e9, -1e9, -2e9, -5e8, 4e9, 1.08e10},
		},
		ExpectedMove1D: &em,
	}

	facts := gexAnalysisFacts(s, "ES")
	for _, want := range []string{
		"all prices below are futures-denominated",
		"spot: 7523.03",
		"total net gamma across the whole book", // named for what it is, not "at spot"
		"expected move (1 session",              // the yardstick
		"x the expected move",                   // distances expressed in EM multiples
		"spot is ABOVE the flip",                // which side of the regime line
		"long-gamma pocket (flip to call wall)",
		"largest positive node: 7558.61 at +10.8B - this IS the call wall", // futures terms + named, so one level never reads as two
		"NO positive-gamma shelf", // the air below the flip
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts missing %q:\n%s", want, facts)
		}
	}
	// The pocket is flip->call wall = 92.79 pts against a 9.74-pt (scaled) EM.
	if !strings.Contains(facts, "92.8 points wide, 9.53x the expected move") {
		t.Errorf("pocket geometry wrong:\n%s", facts)
	}

	// Cold basis: everything falls back to index terms, EM unscaled.
	s.SpotFutures, s.FuturesConversion = nil, nil
	s.Flip.Futures, s.Levels[0].Futures, s.Levels[1].Futures = nil, nil, nil
	facts = gexAnalysisFacts(s, "ES")
	if !strings.Contains(facts, "all prices below are index-denominated") ||
		!strings.Contains(facts, "spot: 7494.55") {
		t.Errorf("cold-basis facts not index-denominated:\n%s", facts)
	}
}
