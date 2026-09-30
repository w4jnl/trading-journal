package gex

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
)

// qualityFixture is snapshotFixture with the v0.11.0 additions spliced in.
func qualityFixture(t *testing.T) string {
	t.Helper()
	extra := `,
  "pc_volume_legs": {"SPY": {"put": 5000000, "call": 4400000, "ratio": 1.1364}, "SPX": {"put": 1321745, "call": 1137231, "ratio": 1.1622}},
  "data_quality": {
    "degraded": true,
    "warnings": [
      {"code": "STALE_LEG", "severity": "degraded", "symbol": "SPY", "message": "SPY leg 18 min older than SPX"},
      {"code": "STALE_PAYLOAD", "severity": "notice", "symbol": null, "message": "payload age 900s"}
    ],
    "funnel": {"raw": 12000, "priced": 11800}, "xcheck": {"median_rel_err": 0.034},
    "rate_source": "fred:SOFR", "snapshot_age_seconds": 912, "quote_effective_at": "2026-07-31T19:35:02+00:00"
  },
  "provenance": {"kind": "reconstructed", "chain_as_of": "2026-07-31T18:50:02.101Z", "spot_source": "mw_marks", "spot_as_of": "2026-07-31T19:50:00Z", "reason": "vendor outage 19:00-20:00"}
}`
	f := strings.TrimSpace(snapshotFixture)
	if !strings.HasSuffix(f, "}") {
		t.Fatal("fixture shape")
	}
	return f[:len(f)-1] + extra
}

func TestSnapshotQualityDecode(t *testing.T) {
	body := qualityFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL})
	s, err := c.Snapshot(context.Background(), "ES", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsDegraded() || s.DataQuality == nil || len(s.DataQuality.Warnings) != 2 {
		t.Fatalf("data_quality = %+v", s.DataQuality)
	}
	dw := s.DataQuality.DegradedWarnings()
	if len(dw) != 1 || dw[0].Code != "STALE_LEG" || dw[0].Symbol != "SPY" {
		t.Errorf("degraded warnings = %+v", dw)
	}
	if got := dw[0].Summary(); got != "STALE_LEG SPY - SPY leg 18 min older than SPX" {
		t.Errorf("summary = %q", got)
	}
	// A null symbol decodes to "", and the notice keeps its severity.
	if s.DataQuality.Warnings[1].Symbol != "" || s.DataQuality.Warnings[1].Severity != "notice" {
		t.Errorf("notice warning = %+v", s.DataQuality.Warnings[1])
	}
	if s.DataQuality.SnapshotAgeSeconds == nil || *s.DataQuality.SnapshotAgeSeconds != 912 {
		t.Errorf("snapshot_age_seconds = %v", s.DataQuality.SnapshotAgeSeconds)
	}
	if !s.DataQuality.QuoteEffectiveAt.Equal(time.Date(2026, 7, 31, 19, 35, 2, 0, time.UTC)) {
		t.Errorf("quote_effective_at = %v (numeric offset must parse)", s.DataQuality.QuoteEffectiveAt)
	}
	if s.ProvenanceKind() != "reconstructed" || s.Provenance.Reason == "" {
		t.Errorf("provenance = %+v", s.Provenance)
	}
	if !s.Provenance.ChainAsOf.Equal(time.Date(2026, 7, 31, 18, 50, 2, 101000000, time.UTC)) {
		t.Errorf("chain_as_of = %v", s.Provenance.ChainAsOf)
	}
	if len(s.PCVolumeLegs) != 2 || s.PCVolumeLegs["SPX"].Ratio != 1.1622 {
		t.Errorf("pc_volume_legs = %+v", s.PCVolumeLegs)
	}
}

// Rows persisted before v0.11.0 serve null for all three additions; the
// contract reads that as measured and not assessed - never degraded.
func TestSnapshotQualityNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"market":"ES","as_of":"2026-07-30T20:00:01Z","spot_index":7400,
			"regime":{"state":"positive"},"flip":{},"levels":[],
			"data_quality":null,"provenance":null,"pc_volume_legs":null}`))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL})
	s, err := c.Snapshot(context.Background(), "ES", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.DataQuality != nil || s.Provenance != nil || s.PCVolumeLegs != nil {
		t.Errorf("nulls not preserved: %+v %+v %+v", s.DataQuality, s.Provenance, s.PCVolumeLegs)
	}
	if s.IsDegraded() || s.ProvenanceKind() != "measured" || s.DataQuality.DegradedWarnings() != nil {
		t.Errorf("nil helpers: degraded=%v kind=%q", s.IsDegraded(), s.ProvenanceKind())
	}
	var nilSnap *Snapshot
	if nilSnap.IsDegraded() || nilSnap.ProvenanceKind() != "measured" {
		t.Error("nil snapshot helpers must be safe")
	}
}

// A garbage timestamp inside the informational objects must not fail the
// snapshot decode - the row's numbers are what matter.
func TestISOTimeTolerant(t *testing.T) {
	var p Provenance
	if err := json.Unmarshal([]byte(`{"kind":"recomputed","chain_as_of":"not a time","spot_as_of":null}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Kind != "recomputed" || !p.ChainAsOf.IsZero() || !p.SpotAsOf.IsZero() {
		t.Errorf("provenance = %+v", p)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"chain_as_of":null`) {
		t.Errorf("zero ISOTime should marshal as null: %s", b)
	}
	var q DataQuality
	if err := json.Unmarshal([]byte(`{"degraded":false,"warnings":[],"quote_effective_at":"2026-09-29T09:35:00-04:00"}`), &q); err != nil {
		t.Fatal(err)
	}
	if !q.QuoteEffectiveAt.Equal(time.Date(2026, 9, 29, 13, 35, 0, 0, time.UTC)) {
		t.Errorf("quote_effective_at = %v", q.QuoteEffectiveAt)
	}
}

// cleanEngine serves a small at-or-before history: rows keyed by as_of with
// a degraded flag, answering each ?at= with the newest row at or before it.
type cleanEngine struct {
	mu   sync.Mutex
	rows []struct {
		asOf     time.Time
		degraded bool
	}
	ats []string
}

func (e *cleanEngine) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		raw := r.URL.Query().Get("at")
		e.ats = append(e.ats, raw)
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			http.Error(w, `{"detail":"bad at"}`, http.StatusUnprocessableEntity)
			return
		}
		for i := len(e.rows) - 1; i >= 0; i-- {
			row := e.rows[i]
			if !row.asOf.After(at) {
				dq := "null"
				if row.degraded {
					dq = `{"degraded":true,"warnings":[{"code":"FROZEN_SPOT","severity":"degraded","symbol":"SPX","message":"spot unchanged 4 cycles"}]}`
				} else {
					dq = `{"degraded":false,"warnings":[]}`
				}
				fmt.Fprintf(w, `{"market":"ES","as_of":%q,"spot_index":7400,"regime":{"state":"positive"},"flip":{},"levels":[],"data_quality":%s}`,
					row.asOf.UTC().Format(time.RFC3339Nano), dq)
				return
			}
		}
		http.Error(w, `{"detail":"no snapshot"}`, http.StatusNotFound)
	}
}

func TestCleanSnapshot(t *testing.T) {
	grid := func(min int, degraded bool) struct {
		asOf     time.Time
		degraded bool
	} {
		// Measured rows land ~2s past the grid mark with microseconds, like
		// the engine's.
		return struct {
			asOf     time.Time
			degraded bool
		}{time.Date(2026, 9, 29, 14, min, 2, 507808000, time.UTC), degraded}
	}
	e := &cleanEngine{}
	e.rows = append(e.rows, grid(0, false), grid(5, true), grid(10, true), grid(15, false), grid(20, true))
	srv := httptest.NewServer(e.handler())
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL})
	ctx := context.Background()

	// Clean first row: one call, no stepping.
	at := time.Date(2026, 9, 29, 14, 16, 0, 0, time.UTC)
	s, skipped, err := c.CleanSnapshot(ctx, "ES", at, 3, 15*time.Minute)
	if err != nil || skipped != 0 || s.AsOf.Minute() != 15 {
		t.Fatalf("clean first: as_of=%v skipped=%d err=%v", s.AsOf, skipped, err)
	}
	if len(e.ats) != 1 {
		t.Errorf("calls = %d, want 1", len(e.ats))
	}

	// Degraded at 14:20 -> 14:15 is clean: one step, addressed exactly one
	// microsecond before the degraded row's as_of.
	e.ats = nil
	at = time.Date(2026, 9, 29, 14, 21, 0, 0, time.UTC)
	s, skipped, err = c.CleanSnapshot(ctx, "ES", at, 3, 15*time.Minute)
	if err != nil || skipped != 1 || s.AsOf.Minute() != 15 || s.IsDegraded() {
		t.Fatalf("one step: as_of=%v skipped=%d degraded=%v err=%v", s.AsOf, skipped, s.IsDegraded(), err)
	}
	if len(e.ats) != 2 || e.ats[1] != "2026-09-29T14:20:02.507807Z" {
		t.Errorf("step addressing = %v", e.ats)
	}

	// Two degraded rows in a row (14:10, 14:05) -> 14:00 clean on the second
	// step, still inside the window.
	at = time.Date(2026, 9, 29, 14, 11, 0, 0, time.UTC)
	s, skipped, err = c.CleanSnapshot(ctx, "ES", at, 3, 15*time.Minute)
	if err != nil || skipped != 2 || s.AsOf.Minute() != 0 {
		t.Fatalf("two steps: as_of=%v skipped=%d err=%v", s.AsOf, skipped, err)
	}

	// Window too tight: the clean row would be older than `within`, so the
	// degraded row the engine served comes back, flagged.
	s, skipped, err = c.CleanSnapshot(ctx, "ES", at, 3, 8*time.Minute)
	if err != nil || skipped != 0 || s.AsOf.Minute() != 10 || !s.IsDegraded() {
		t.Fatalf("window: as_of=%v skipped=%d degraded=%v err=%v", s.AsOf, skipped, s.IsDegraded(), err)
	}

	// Step budget exhausted (maxSteps 1) -> the first row again.
	s, skipped, err = c.CleanSnapshot(ctx, "ES", at, 1, 15*time.Minute)
	if err != nil || skipped != 0 || s.AsOf.Minute() != 10 {
		t.Fatalf("budget: as_of=%v skipped=%d err=%v", s.AsOf, skipped, err)
	}

	// Nothing before the requested moment: the 404 propagates as ErrNoSnapshot.
	if _, _, err := c.CleanSnapshot(ctx, "ES", time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC), 3, 15*time.Minute); err != ErrNoSnapshot {
		t.Errorf("pre-history err = %v", err)
	}
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"status":"degraded","now":"2026-09-29T11:17:14+00:00",
			"reasons":["vendor host unreachable since 13:52Z", {"code":"NO_CYCLE","message":"no successful cycle this session"}, {"kind":"x","count":3}],
			"notices":[" stale marks "],
			"scheduler":{"running":true}}`))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k"})
	hl, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hl.Status != "degraded" {
		t.Errorf("status = %q", hl.Status)
	}
	want := []string{"vendor host unreachable since 13:52Z", "no successful cycle this session", "count=3 kind=x"}
	if strings.Join(hl.Reasons, "|") != strings.Join(want, "|") {
		t.Errorf("reasons = %q", hl.Reasons)
	}
	if len(hl.Notices) != 1 || hl.Notices[0] != "stale marks" {
		t.Errorf("notices = %q", hl.Notices)
	}
	c2, _ := New(Options{BaseURL: srv.URL, APIKey: "wrong"})
	if _, err := c2.Health(context.Background()); err != ErrUnauthorized {
		t.Errorf("unauthorized err = %v", err)
	}
}
