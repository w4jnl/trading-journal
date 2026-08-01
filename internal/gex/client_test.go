package gex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// snapshotFixture is a real dev-engine response (2026-07-31, abridged profile).
const snapshotFixture = `{
  "market": "ES",
  "as_of": "2026-07-31T19:50:02.722581Z",
  "sources": ["SPX", "SPY"],
  "spot_index": 7494.5498,
  "oi_date": "2026-07-31",
  "futures_conversion": {
    "ratio": 1.0037961186140893, "source": "mw_marks",
    "paired_at": "2026-07-31T19:34:39Z", "age_seconds": 921
  },
  "regime": { "state": "positive", "net_gex_at_spot": 74486642363.04909, "percentile_20d": null },
  "flip": { "index": 7437.554700050995, "futures": 7465.788539891167 },
  "levels": [
    { "type": "call_wall", "index": 7530.0, "futures": 7558.584773164092, "gex": 10838901323.0, "bucket": "all" },
    { "type": "put_wall",  "index": 7300.0, "futures": 7327.711665882853, "gex": -3897046844.0, "bucket": "all" }
  ],
  "profile": { "strikes_index": [7300.0, 7530.0], "net_gex": [-3.9e9, 1.08e10] },
  "expected_move_1d": 9.7
}`

func TestSnapshot(t *testing.T) {
	var gotPath, gotAt, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAt = r.URL.Query().Get("at")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(snapshotFixture))
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL + "/", APIKey: "sekrit", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// A zoned non-UTC time must be sent as UTC with the Z suffix (the engine
	// 422s naive timestamps; at-or-before needs the true instant).
	at := time.Date(2026, 7, 31, 15, 55, 0, 0, time.FixedZone("ET", -4*3600))
	s, err := c.Snapshot(context.Background(), "ES", at)
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/v1/snapshots/ES" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAt != "2026-07-31T19:55:00Z" {
		t.Errorf("at = %q, want UTC Z form", gotAt)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("auth = %q", gotAuth)
	}

	if s.Market != "ES" || s.Regime.State != "positive" {
		t.Errorf("market/regime = %q/%q", s.Market, s.Regime.State)
	}
	if !s.AsOf.Equal(time.Date(2026, 7, 31, 19, 50, 2, 722581000, time.UTC)) {
		t.Errorf("as_of = %v", s.AsOf)
	}
	if s.Regime.Percentile20D != nil {
		t.Errorf("percentile = %v, want nil (engine null)", *s.Regime.Percentile20D)
	}
	if s.Flip.Futures == nil || *s.Flip.Futures != 7465.788539891167 {
		t.Errorf("flip.futures = %v", s.Flip.Futures)
	}
	if len(s.Levels) != 2 || s.Levels[0].Type != "call_wall" || s.Levels[0].Futures == nil {
		t.Errorf("levels = %+v", s.Levels)
	}
	if s.ExpectedMove1D == nil || *s.ExpectedMove1D != 9.7 {
		t.Errorf("expected_move_1d = %v", s.ExpectedMove1D)
	}
	if len(s.Profile.StrikesIndex) != 2 || s.Profile.StrikesIndex[0] != 7300.0 ||
		len(s.Profile.NetGex) != 2 || s.Profile.NetGex[1] != 1.08e10 {
		t.Errorf("profile = %+v", s.Profile)
	}
}

// Null flip / empty levels / cold conversion are legitimate states, not
// decode errors.
func TestSnapshotNullFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"market": "NQ", "as_of": "2026-07-30T20:00:01Z", "spot_index": 28000,
			"futures_conversion": null,
			"regime": { "state": "negative", "net_gex_at_spot": -1e9, "percentile_20d": 37.5 },
			"flip": { "index": null, "futures": null },
			"levels": [], "expected_move_1d": null
		}`))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	s, err := c.Snapshot(context.Background(), "NQ", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.FuturesConversion != nil || s.Flip.Futures != nil || len(s.Levels) != 0 || s.ExpectedMove1D != nil {
		t.Errorf("null fields not preserved: %+v", s)
	}
	if s.Regime.Percentile20D == nil || *s.Regime.Percentile20D != 37.5 {
		t.Errorf("percentile = %v", s.Regime.Percentile20D)
	}
}

func TestSnapshotStatusMapping(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":"nope"}`))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL})

	if _, err := c.Snapshot(context.Background(), "ES", time.Now()); !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("404 -> %v, want ErrNoSnapshot", err)
	}
	status = http.StatusUnauthorized
	if _, err := c.Snapshot(context.Background(), "ES", time.Now()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("401 -> %v, want ErrUnauthorized", err)
	}
	status = http.StatusForbidden
	if _, err := c.Snapshot(context.Background(), "ES", time.Now()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("403 -> %v, want ErrUnauthorized", err)
	}
	status = http.StatusUnprocessableEntity
	_, err := c.Snapshot(context.Background(), "ES", time.Now())
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("422 -> %v, want formatted error with body snippet", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without BaseURL should fail")
	}
	c, err := New(Options{BaseURL: "http://gex-engine:8420"})
	if err != nil {
		t.Fatal(err)
	}
	if c.http.Timeout != 5*time.Second {
		t.Errorf("default timeout = %v, want 5s", c.http.Timeout)
	}
}
