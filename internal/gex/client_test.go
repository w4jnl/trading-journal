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
  "spot_futures": 7523.0,
  "oi_date": "2026-07-31",
  "futures_conversion": {
    "ratio": 1.0037961186140893, "source": "mw_marks",
    "paired_at": "2026-07-31T19:34:39Z", "age_seconds": 921
  },
  "regime": { "state": "positive", "net_gex_at_spot": 74486642363.04909, "percentile_20d": null, "intraday_state": "negative" },
  "flip": { "index": 7437.554700050995, "futures": 7465.788539891167 },
  "flip_0dte": { "index": 7460.0, "futures": 7488.35 },
  "flip_intraday": { "index": 7470.0, "futures": 7498.39 },
  "levels": [
    { "type": "call_wall", "index": 7530.0, "futures": 7558.584773164092, "gex": 10838901323.0, "bucket": "all" },
    { "type": "put_wall",  "index": 7300.0, "futures": 7327.711665882853, "gex": -3897046844.0, "bucket": "all" },
    { "type": "gamma_wall", "index": 7500.0, "futures": 7528.5, "gex": 1.5e9, "bucket": "0dte" }
  ],
  "profile": { "strikes_index": [7300.0, 7530.0], "net_gex": [-3.9e9, 1.08e10] },
  "net_gex_change_1h": -9.2e9,
  "net_gex_change_1d": 11.3e9,
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
	// Sub-second precision must survive: passing a snapshot's own as_of back
	// in has to address THAT snapshot, not its predecessor.
	exact := time.Date(2026, 7, 31, 19, 0, 2, 501055000, time.UTC)
	if _, err := c.Snapshot(context.Background(), "ES", exact); err != nil {
		t.Fatal(err)
	}
	if gotAt != "2026-07-31T19:00:02.501055Z" {
		t.Errorf("at = %q, want the fractional second preserved", gotAt)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("auth = %q", gotAuth)
	}

	if s.Market != "ES" || s.Regime.State != "positive" {
		t.Errorf("market/regime = %q/%q", s.Market, s.Regime.State)
	}
	if s.SpotFutures == nil || *s.SpotFutures != 7523.0 {
		t.Errorf("spot_futures = %v (engine-published, not derived)", s.SpotFutures)
	}
	if !s.AsOf.Equal(time.Date(2026, 7, 31, 19, 50, 2, 722581000, time.UTC)) {
		t.Errorf("as_of = %v", s.AsOf)
	}
	if s.Regime.Percentile20D != nil {
		t.Errorf("percentile = %v, want nil (engine null)", *s.Regime.Percentile20D)
	}
	if s.Regime.IntradayState != "negative" {
		t.Errorf("intraday_state = %q", s.Regime.IntradayState)
	}
	if s.Flip.Futures == nil || *s.Flip.Futures != 7465.788539891167 {
		t.Errorf("flip.futures = %v", s.Flip.Futures)
	}
	if len(s.Levels) != 3 || s.Levels[0].Type != "call_wall" || s.Levels[0].Futures == nil {
		t.Errorf("levels = %+v", s.Levels)
	}
	if s.Levels[2].Bucket != "0dte" {
		t.Errorf("levels[2].bucket = %q, want 0dte decoded", s.Levels[2].Bucket)
	}
	if s.Flip0DTE.Index == nil || *s.Flip0DTE.Index != 7460.0 {
		t.Errorf("flip_0dte.index = %v", s.Flip0DTE.Index)
	}
	if s.FlipIntraday.Futures == nil || *s.FlipIntraday.Futures != 7498.39 {
		t.Errorf("flip_intraday.futures = %v", s.FlipIntraday.Futures)
	}
	if s.NetGexChange1H == nil || *s.NetGexChange1H != -9.2e9 ||
		s.NetGexChange1D == nil || *s.NetGexChange1D != 11.3e9 {
		t.Errorf("net_gex_change 1h/1d = %v/%v", s.NetGexChange1H, s.NetGexChange1D)
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
	// Pre-v0.5.0 payload: every update-002 field stays nil, never zero.
	if s.Flip0DTE.Index != nil || s.Flip0DTE.Futures != nil ||
		s.FlipIntraday.Index != nil || s.FlipIntraday.Futures != nil ||
		s.NetGexChange1H != nil || s.NetGexChange1D != nil {
		t.Errorf("absent v0.5.0 fields not nil: %+v", s)
	}
	if s.Regime.Percentile20D == nil || *s.Regime.Percentile20D != 37.5 {
		t.Errorf("percentile = %v", s.Regime.Percentile20D)
	}
	if s.Regime.IntradayState != "" {
		t.Errorf("absent intraday_state = %q, want empty", s.Regime.IntradayState)
	}
}

// coldBasisFixture is a REAL engine v0.3.0 response captured against a copy
// of the live database with basis_ratios emptied (supplied by the engine in
// docs/p4-engine-response.md §3.5). Cold basis nulls the futures fields only:
// index values, regime, profile and expected move stay populated, so a
// consumer must degrade to index-only rather than to "no data".
const coldBasisFixture = `{
  "market": "ES",
  "as_of": "2026-07-31T20:00:03.058037Z",
  "sources": ["SPX", "SPY"],
  "spot_index": 7503.54,
  "spot_futures": null,
  "oi_date": "2026-07-31",
  "futures_conversion": null,
  "regime": { "state": "positive", "net_gex_at_spot": 44964082924.8574, "percentile_20d": null },
  "flip": { "index": 7442.540346680423, "futures": null },
  "levels": [
    { "type": "call_wall", "index": 7550.0, "futures": null, "gex": 5406021999.0, "bucket": "all" },
    { "type": "put_wall",  "index": 7300.0, "futures": null, "gex": -3793769564.0, "bucket": "all" }
  ],
  "profile": { "strikes_index": [6760.0, 6770.0], "net_gex": [-33493001.0, -131439900.0] },
  "expected_move_1d": 41.7
}`

func TestSnapshotColdBasis(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(coldBasisFixture))
	}))
	defer srv.Close()

	c, _ := New(Options{BaseURL: srv.URL})
	s, err := c.Snapshot(context.Background(), "ES", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.SpotFutures != nil || s.FuturesConversion != nil || s.Flip.Futures != nil {
		t.Errorf("futures fields not null under cold basis: %+v", s)
	}
	for _, l := range s.Levels {
		if l.Futures != nil {
			t.Errorf("%s futures not null: %v", l.Type, *l.Futures)
		}
	}
	// Everything index-denominated survives.
	if s.SpotIndex != 7503.54 || s.Flip.Index == nil || *s.Flip.Index != 7442.540346680423 {
		t.Errorf("index values lost: spot %v flip %v", s.SpotIndex, s.Flip.Index)
	}
	if s.Regime.State != "positive" || s.ExpectedMove1D == nil || *s.ExpectedMove1D != 41.7 {
		t.Errorf("regime/EM lost: %+v", s.Regime)
	}
	if len(s.Profile.StrikesIndex) != 2 || len(s.Profile.NetGex) != 2 {
		t.Errorf("profile lost: %+v", s.Profile)
	}
}

// net_gex_total is the current field; net_gex_at_spot is a deprecated alias
// carrying the identical value (the old name misdescribed it - nothing is
// evaluated at spot). Prefer the new name, fall back to the alias so a
// pre-v0.3.1 engine still renders, and report absence honestly.
func TestRegimeTotal(t *testing.T) {
	cases := []struct {
		name   string
		regime Regime
		want   float64
		ok     bool
	}{
		{"both present (current engine)", Regime{NetGexTotal: p(45.0e9), NetGexAtSpot: p(45.0e9)}, 45.0e9, true},
		{"alias only (older engine)", Regime{NetGexAtSpot: p(45.0e9)}, 45.0e9, true},
		{"total only (alias retired)", Regime{NetGexTotal: p(45.0e9)}, 45.0e9, true},
		{"genuine zero is not absence", Regime{NetGexTotal: p(0)}, 0, true},
		{"neither", Regime{}, 0, false},
	}
	for _, c := range cases {
		got, ok := c.regime.Total()
		if got != c.want || ok != c.ok {
			t.Errorf("%s: = %v,%v want %v,%v", c.name, got, ok, c.want, c.ok)
		}
	}

	// And it decodes from an older engine's payload, which omits the new key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"market":"ES","as_of":"2026-07-31T20:00:03Z","spot_index":7503.54,
			"regime":{"state":"positive","net_gex_at_spot":44964082924.8574,"percentile_20d":null},
			"flip":{"index":null,"futures":null},"levels":[]}`))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL})
	s, err := c.Snapshot(context.Background(), "ES", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if total, ok := s.Regime.Total(); !ok || total != 44964082924.8574 {
		t.Errorf("pre-v0.3.1 payload = %v,%v", total, ok)
	}
}

func p(v float64) *float64 { return &v }

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

// profileFixture is an abridged live-shape /v1/profile response.
const profileFixture = `{
  "market": "ES", "as_of": "2026-07-31T19:50:02.722581Z", "oi_date": "2026-07-31",
  "spot_index": 7494.5498, "bucket_width": 10.0, "sources": ["SPX", "SPY"],
  "profiles": {
    "blend": {"strikes_index": [7300.0, 7530.0], "net_gex": [-3.9e9, 1.08e10], "call_gex": [1.0e9, 1.2e10], "put_gex": [-4.9e9, -1.2e9]},
    "0dte":  {"strikes_index": [7450.0], "net_gex": [1.1e9], "call_gex": [1.5e9], "put_gex": [-0.4e9]}
  },
  "expiry_decay": {"2026-07-31": -6.0e9, "2026-08-21": 4.0e9}
}`

func TestProfileDetail(t *testing.T) {
	var gotPath, gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotQuery = r.URL.Path, r.Header.Get("Authorization"), r.URL.RawQuery
		_, _ = w.Write([]byte(profileFixture))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "sekrit"})
	pd, err := c.ProfileDetail(context.Background(), "ES", ProfileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/profile/ES" || gotAuth != "Bearer sekrit" || gotQuery != "" {
		t.Errorf("path/auth/query = %q / %q / %q", gotPath, gotAuth, gotQuery)
	}
	if !pd.AsOf.Equal(time.Date(2026, 7, 31, 19, 50, 2, 722581000, time.UTC)) {
		t.Errorf("as_of = %v", pd.AsOf)
	}
	if pd.BucketWidth != 10.0 || pd.OIDate != "2026-07-31" {
		t.Errorf("bucket/oi = %v / %q", pd.BucketWidth, pd.OIDate)
	}
	b, ok := pd.Profiles["blend"]
	if !ok || len(b.StrikesIndex) != 2 || b.CallGex[1] != 1.2e10 || b.PutGex[0] != -4.9e9 {
		t.Errorf("blend = %+v", b)
	}
	if _, ok := pd.Profiles["0dte"]; !ok {
		t.Errorf("0dte profile missing")
	}
	if pd.ExpiryDecay["2026-08-21"] != 4.0e9 {
		t.Errorf("decay = %v", pd.ExpiryDecay)
	}
	// Pre-v0.7.0 fixture: no expiry_filter key, no OI, no futures fields.
	if pd.SupportsExpiry || pd.SpotFutures != nil || b.CallOI != nil {
		t.Errorf("legacy fixture: supports=%v spotFut=%v callOI=%v", pd.SupportsExpiry, pd.SpotFutures, b.CallOI)
	}

	// The same legacy engine would silently ignore ?expiry= - the client
	// must refuse to present the whole book as a filtered slice.
	if _, err := c.ProfileDetail(context.Background(), "ES", ProfileQuery{Expiries: []string{"2026-08-21"}}); !errors.Is(err, ErrExpiryUnsupported) {
		t.Errorf("expiry against legacy engine -> %v, want ErrExpiryUnsupported", err)
	}
}

// profileFixtureV070 carries the v0.7.0 additions: expiry_filter echo,
// per-strike OI, and the futures denomination fields.
const profileFixtureV070 = `{
  "market": "ES", "as_of": "2026-08-20T14:00:03.2Z", "oi_date": "2026-08-20",
  "spot_index": 7494.5498, "bucket_width": 10.0, "sources": ["SPX", "SPY"],
  "spot_futures": 7523.2, "futures_conversion": {"ratio": 1.0038, "source": "persisted", "paired_at": "2026-07-31T19:44:39Z", "age_seconds": 60},
  "expiry_filter": ["2026-08-21"],
  "profiles": {
    "blend": {"strikes_index": [7300.0, 7530.0], "net_gex": [-1.0e9, 2.0e9], "call_gex": [0.5e9, 2.5e9], "put_gex": [-1.5e9, -0.5e9], "call_oi": [1200, 3400], "put_oi": [5600, 780]}
  },
  "expiry_decay": null
}`

func TestProfileDetailV070(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(profileFixtureV070))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL})
	at := time.Date(2026, 8, 20, 14, 0, 59, 0, time.UTC)
	pd, err := c.ProfileDetail(context.Background(), "ES", ProfileQuery{At: at, Expiries: []string{"2026-08-21"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "at=2026-08-20T14%3A00%3A59Z") || !strings.Contains(gotQuery, "expiry=2026-08-21") {
		t.Errorf("query = %q", gotQuery)
	}
	if !pd.SupportsExpiry || len(pd.ExpiryFilter) != 1 || pd.ExpiryFilter[0] != "2026-08-21" {
		t.Errorf("filter echo = %v (supports %v)", pd.ExpiryFilter, pd.SupportsExpiry)
	}
	if pd.SpotFutures == nil || *pd.SpotFutures != 7523.2 || pd.FuturesConversion.Ratio != 1.0038 {
		t.Errorf("futures fields = %v / %+v", pd.SpotFutures, pd.FuturesConversion)
	}
	b := pd.Profiles["blend"]
	if len(b.CallOI) != 2 || b.CallOI[1] != 3400 || b.PutOI[0] != 5600 {
		t.Errorf("OI = %v / %v", b.CallOI, b.PutOI)
	}

	// An unfiltered v0.7.0 response has "expiry_filter": null - the KEY
	// alone must flip the capability probe.
	nullFilter := strings.Replace(profileFixtureV070, `["2026-08-21"]`, "null", 1)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(nullFilter))
	}))
	defer srv2.Close()
	c2, _ := New(Options{BaseURL: srv2.URL})
	pd2, err := c2.ProfileDetail(context.Background(), "ES", ProfileQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !pd2.SupportsExpiry || pd2.ExpiryFilter != nil {
		t.Errorf("null filter: supports=%v echo=%v", pd2.SupportsExpiry, pd2.ExpiryFilter)
	}
}

func TestProfileDetailBucket(t *testing.T) {
	// The served bucket_width must echo the request: a match succeeds, a
	// mismatch (an old engine silently ignoring ?bucket=) is ErrBadBucket,
	// and the engine's explicit 422 maps to the same sentinel.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("bucket") {
		case "20":
			_, _ = w.Write([]byte(strings.Replace(profileFixtureV070, `"bucket_width": 10.0`, `"bucket_width": 20.0`, 1)))
		case "7":
			http.Error(w, `{"detail":"bucket must be a multiple of this snapshot's stored width 10, at most 100"}`, http.StatusUnprocessableEntity)
		default:
			_, _ = w.Write([]byte(profileFixtureV070)) // ignores the param
		}
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL})
	pd, err := c.ProfileDetail(context.Background(), "ES", ProfileQuery{Bucket: 20, Expiries: []string{"2026-08-21"}})
	if err != nil || pd.BucketWidth != 20 {
		t.Fatalf("bucket=20 -> %v / width %v", err, pd)
	}
	if _, err := c.ProfileDetail(context.Background(), "ES", ProfileQuery{Bucket: 7}); !errors.Is(err, ErrBadBucket) || !strings.Contains(err.Error(), "stored width") {
		t.Errorf("engine 422 -> %v, want ErrBadBucket with detail", err)
	}
	if _, err := c.ProfileDetail(context.Background(), "ES", ProfileQuery{Bucket: 50}); !errors.Is(err, ErrBadBucket) {
		t.Errorf("silently ignored bucket -> %v, want ErrBadBucket", err)
	}
}

func TestProfileDetailErrors(t *testing.T) {
	// The engine's three 404s share the endpoint; the detail string is the
	// discriminator (verified against v0.7.0).
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusUnauthorized, "no", ErrUnauthorized},
		{http.StatusNotFound, "no", ErrNoProfile},
		{http.StatusNotFound, `{"detail":"no per-expiry data for this snapshot"}`, ErrNoExpiryData},
		{http.StatusNotFound, `{"detail":"no snapshot at or before that time"}`, ErrNoSnapshot},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, tc.body, tc.status)
		}))
		c, _ := New(Options{BaseURL: srv.URL})
		if _, err := c.ProfileDetail(context.Background(), "ES", ProfileQuery{}); !errors.Is(err, tc.want) {
			t.Errorf("status %d body %q -> %v, want %v", tc.status, tc.body, err, tc.want)
		}
		srv.Close()
	}
	// Non-sentinel statuses surface a snippet error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL})
	if _, err := c.ProfileDetail(context.Background(), "ES", ProfileQuery{}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500 -> %v", err)
	}
}
