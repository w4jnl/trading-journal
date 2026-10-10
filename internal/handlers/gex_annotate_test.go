package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/gexview"
	"github.com/w4jnl/trading-journal/internal/models"
)

// gexTestDB opens a fully migrated database (the real schema, so column
// drift between hand-written DDL and the migrations cannot hide) and seeds
// what every GEX test assumes: users 7 and 8, one account each, and user 7's
// closed MES trade 42 (the window gexTestTrade uses). Instruments come from
// the seed migration (MES tick 0.25).
func gexTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := dbtest.Open(t, "gexboot")
	tr := gexTestTrade("MES")
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, username, display_name) VALUES (7, 'seven', 'Seven'), (8, 'eight', 'Eight')`, nil},
		{`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES
			(1, 7, 'Apex 1', 'motivewave_csv', 'APEX-1'), (2, 8, 'Someone else', 'motivewave_csv', 'APEX-2')`, nil},
		{`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_time, entry_price, exit_time, exit_price, status, net_pnl)
			VALUES (?, 1, ?, ?, ?, 1, ?, ?, ?, ?, 'closed', 12.5)`,
			[]any{tr.ID, tr.Symbol, tr.SymbolRoot, tr.Direction, tr.EntryTime, tr.EntryPrice, tr.ExitTime, *tr.ExitPrice}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func gexSeedEntry(t *testing.T, db *sql.DB, id int64, body string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (?, 7, ?, ?, 'trade')`,
		id, body, time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
}

func gexEntryBody(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var b string
	if err := db.QueryRow(`SELECT body FROM journal_entries WHERE id = ?`, id).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// gexSnapshotJSON is a realistic ES snapshot payload with a configurable
// as_of, shared by the annotate and page tests. It carries the full engine
// v0.5.0 level menu (update 002), with the 0DTE walls listed BEFORE the
// all-book ones so every consumer path exercises the bucket guard.
func gexSnapshotJSON(asOf string) string {
	return fmt.Sprintf(`{
		"market": "ES", "as_of": %q, "spot_index": 7494.55,
		"futures_conversion": {"ratio": 1.0038, "source": "mw_marks", "paired_at": "2026-07-31T19:34:39Z"},
		"regime": {"state": "positive", "net_gex_at_spot": 7.4486642363e10, "percentile_20d": null, "intraday_state": "negative", "flipped_at": "2026-07-31T18:10:03Z", "intraday_flipped_at": "2026-07-31T17:40:03Z"},
		"flip": {"index": 7437.55, "futures": 7465.788539891167},
		"flip_0dte": {"index": 7460.0, "futures": 7488.35},
		"flip_intraday": {"index": 7470.0, "futures": 7498.39},
		"levels": [
			{"type": "call_wall", "index": 7510.0, "futures": 7538.54, "gex": 2.1e9, "bucket": "0dte"},
			{"type": "put_wall", "index": 7450.0, "futures": 7478.31, "gex": -1.2e9, "bucket": "0dte"},
			{"type": "gamma_wall", "index": 7500.0, "futures": 7528.5, "gex": 1.5e9, "bucket": "0dte"},
			{"type": "call_wall", "index": 7505.0, "futures": 7533.52, "gex": 1.9e9, "bucket": "0dte_vol"},
			{"type": "put_wall", "index": 7445.0, "futures": 7473.28, "gex": -1.1e9, "bucket": "0dte_vol"},
			{"type": "gamma_wall", "index": 7505.0, "futures": 7533.52, "gex": 1.9e9, "bucket": "0dte_vol"},
			{"type": "gamma_near_1", "index": 7490.0, "futures": 7518.46, "gex": 5.2e9, "bucket": "all"},
			{"type": "gamma_near_2", "index": 7470.0, "futures": 7498.39, "gex": 3.1e9, "bucket": "all"},
			{"type": "gamma_near_3", "index": 7510.0, "futures": 7538.54, "gex": 2.9e9, "bucket": "all"},
			{"type": "call_wall", "index": 7530.0, "futures": 7558.584773164092, "gex": 1.08e10, "bucket": "all"},
			{"type": "put_wall", "index": 7300.0, "futures": 7327.711665882853, "gex": -3.9e9, "bucket": "all"},
			{"type": "gamma_1", "index": 7500.0, "futures": 7528.5, "gex": 8.4e9, "bucket": "all"},
			{"type": "gamma_2", "index": 7450.0, "futures": 7478.31, "gex": 6.1e9, "bucket": "all"},
			{"type": "gamma_3", "index": 7480.0, "futures": 7508.42, "gex": 4.4e9, "bucket": "all"},
			{"type": "em_1d_min", "index": 7455.3, "futures": 7483.63, "gex": 0.0, "bucket": "all"},
			{"type": "em_1d_max", "index": 7519.7, "futures": 7548.29, "gex": 0.0, "bucket": "all"},
			{"type": "em_intra_min", "index": 7460.2, "futures": 7488.55, "gex": 0.0, "bucket": "all"},
			{"type": "em_intra_max", "index": 7528.9, "futures": 7557.51, "gex": 0.0, "bucket": "all"}
		],
		"profile": {"strikes_index": [7300, 7530], "net_gex": [-3.9e9, 1.08e10]},
		"net_gex_change_1h": -9.2e9,
		"net_gex_change_1d": 1.13e10,
		"expected_move_1d": 9.7,
		"net_dex": 9.786e11, "net_dex_change_1h": 5.18e10,
		"pc_volume": {"put": 6321745, "call": 5537231, "ratio": 1.1417},
		"pc_volume_0dte": {"put": 890100, "call": 720400, "ratio": 1.2356},
		"skew": {"dte0_pct": -6.1, "m1_pct": 29.0}
	}`, asOf)
}

// gexFakeEngine serves gexSnapshotJSON and counts requests.
func gexFakeEngine(asOf string, calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, gexSnapshotJSON(asOf))
	}))
}

func gexTestHandlers(t *testing.T, db *sql.DB, engineURL string) *Handlers {
	t.Helper()
	c, err := gex.New(gex.Options{BaseURL: engineURL, APIKey: "test", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return &Handlers{DB: db, Queries: models.New(db), Timezone: time.UTC, GEX: c}
}

func gexTestTrade(root string) prepDayTrade {
	exitPrice := 7515.25
	return prepDayTrade{
		ID: 42, Symbol: root + "U6", SymbolRoot: root, Direction: "long",
		// 15:52 / 15:58 ET - inside the options session, ~2-8 min after the
		// fake snapshot's as_of, so neither moment is stale.
		EntryTime:  time.Date(2026, 7, 31, 19, 52, 0, 0, time.UTC),
		EntryPrice: 7510.00,
		ExitTime:   time.Date(2026, 7, 31, 19, 58, 0, 0, time.UTC),
		ExitPrice:  &exitPrice,
	}
}

func TestGexAnnotate(t *testing.T) {
	db := gexTestDB(t)
	var calls atomic.Int64
	srv := gexFakeEngine("2026-07-31T19:50:02.722581Z", &calls)
	defer srv.Close()
	h := gexTestHandlers(t, db, srv.URL)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	gexSeedEntry(t, db, 1, aiPlaceholderBody)
	ann := &gexAnnotator{h: h}
	ann.annotate(ctx, 1, gexTestTrade("MES"))

	body := gexEntryBody(t, db, 1)
	if calls.Load() != 2 {
		t.Fatalf("engine calls = %d, want 2 (entry + exit)", calls.Load())
	}
	// Seed-replaced the placeholder, visible block + distances present.
	if !strings.HasPrefix(body, "---\n\n_Gamma context (GEX) — futures prices_") {
		t.Errorf("gamma block did not seed-replace the placeholder:\n%s", body)
	}
	if strings.Contains(body, "_AI review_") {
		t.Errorf("stale placeholder left in body:\n%s", body)
	}
	for _, want := range []string{
		"entry 15:52 ET", "exit 15:58 ET",
		"positive (P—) · intraday negative", // dual reading when the books disagree (update 004)
		"flip 7465.79", "above (+177 t)",    // (7510-7465.79)/0.25
		// The front-week flip row (engine v0.6.0): (7510-7498.39)/0.25.
		"flip intraday 7498.39", "above (+46 t)", "above (+67 t)",
		"call wall 7558.58", "-194 t", // entry moment
		"put wall 7327.71", "+729 t",
		"market=ES", "regime=positive", "pctl=null", "flip_side=above",
		"dist_flip_ticks=+176.8",
		"intraday_regime=negative",
		"flip_intraday_side=above", "dist_flip_intraday_ticks=+46.4",
		"dist_cw_ticks=-194.3", "dist_pw_ticks=+729.2",
		"em=9.7", "stale=false", "<!-- " + gexMarker,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "stale:") {
		t.Errorf("unexpected stale label for in-session trade:\n%s", body)
	}

	// Idempotent: a fresh pass sees the marker and never re-fetches.
	(&gexAnnotator{h: h}).annotate(ctx, 1, gexTestTrade("MES"))
	if calls.Load() != 2 {
		t.Errorf("re-annotate fetched again: calls = %d", calls.Load())
	}
	if got := gexEntryBody(t, db, 1); got != body {
		t.Errorf("re-annotate changed the body")
	}

	// Non-ES/NQ roots produce no gamma noise and no fetches.
	gexSeedEntry(t, db, 2, aiPlaceholderBody)
	(&gexAnnotator{h: h}).annotate(ctx, 2, gexTestTrade("CL"))
	if calls.Load() != 2 {
		t.Errorf("CL trade hit the engine: calls = %d", calls.Load())
	}
	if got := gexEntryBody(t, db, 2); got != aiPlaceholderBody {
		t.Errorf("CL entry body changed: %q", got)
	}
}

// A pre-history moment (engine 404) skips silently without tripping the
// annotator; a transport error trips it so one pass costs one timeout.
func TestGexAnnotateErrors(t *testing.T) {
	db := gexTestDB(t)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"no snapshot"}`, http.StatusNotFound)
	}))
	defer srv404.Close()
	h := gexTestHandlers(t, db, srv404.URL)
	gexSeedEntry(t, db, 1, aiPlaceholderBody)
	ann := &gexAnnotator{h: h}
	ann.annotate(ctx, 1, gexTestTrade("MES"))
	if ann.broken {
		t.Error("404 must not trip the annotator (expected pre-history skip)")
	}
	if got := gexEntryBody(t, db, 1); got != aiPlaceholderBody {
		t.Errorf("404 still annotated: %q", got)
	}

	// Engine down: first trade warns and trips, the rest of the pass skips.
	srvDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvDown.Close()
	h2 := gexTestHandlers(t, db, srvDown.URL)
	ann2 := &gexAnnotator{h: h2}
	ann2.annotate(ctx, 1, gexTestTrade("MES"))
	if !ann2.broken {
		t.Error("transport error did not trip the annotator")
	}
	ann2.annotate(ctx, 1, gexTestTrade("MES")) // must return without dialing
	if got := gexEntryBody(t, db, 1); got != aiPlaceholderBody {
		t.Errorf("broken annotator wrote a body: %q", got)
	}
}

// An overnight/pre-open trade gets the prior session's close snapshot; the
// block must say so instead of presenting the levels as live.
func TestGexAnnotateStale(t *testing.T) {
	db := gexTestDB(t)
	var calls atomic.Int64
	// Snapshot from the prior session close, trade the next morning.
	srv := gexFakeEngine("2026-07-30T20:00:02Z", &calls)
	defer srv.Close()
	h := gexTestHandlers(t, db, srv.URL)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	gexSeedEntry(t, db, 1, aiPlaceholderBody)
	tr := gexTestTrade("MES")
	tr.EntryTime = time.Date(2026, 7, 31, 7, 0, 0, 0, time.UTC) // 03:00 ET
	tr.ExitTime = time.Date(2026, 7, 31, 7, 30, 0, 0, time.UTC)
	(&gexAnnotator{h: h}).annotate(ctx, 1, tr)

	body := gexEntryBody(t, db, 1)
	for _, want := range []string{
		"_entry context stale: levels as of Jul 30 16:00 ET (prior session)_",
		"_exit context stale:",
		"stale=true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestGexMarket(t *testing.T) {
	cases := []struct {
		root, market string
		ok           bool
	}{
		{"ES", "ES", true}, {"MES", "ES", true}, {"EP", "ES", true},
		{"NQ", "NQ", true}, {"MNQ", "NQ", true}, {"mes", "ES", true},
		{"CL", "", false}, {"MCL", "", false}, {"GC", "", false}, {"", "", false},
	}
	for _, c := range cases {
		m, ok := gexMarket(c.root)
		if m != c.market || ok != c.ok {
			t.Errorf("gexMarket(%q) = %q,%v want %q,%v", c.root, m, ok, c.market, c.ok)
		}
	}
}

func TestGexWallBucketGuard(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	s := &gex.Snapshot{Levels: []gex.Level{
		// Engine v0.5.0 lists the 0DTE book's walls alongside the all-book
		// ones; ordering is not contractual, so the 0dte entry comes first
		// here to prove selection is by bucket, not position.
		{Type: "call_wall", Bucket: "0dte", Index: 7400, Futures: f(7428)},
		{Type: "call_wall", Bucket: "all", Index: 7530, Futures: f(7558)},
		{Type: "put_wall", Bucket: "0dte", Index: 7350, Futures: f(7378)},
	}}
	if l := gexview.Wall(s, "call_wall"); l == nil || l.Bucket != "all" || l.Index != 7530 {
		t.Errorf("call_wall = %+v, want the all-bucket level", l)
	}
	// A wall present only in the 0DTE book is not the all-book wall - absent
	// beats wrong.
	if l := gexview.Wall(s, "put_wall"); l != nil {
		t.Errorf("put_wall = %+v, want nil (only 0dte present)", l)
	}
}

func TestGexTickSizeFallback(t *testing.T) {
	db := gexTestDB(t)
	h := &Handlers{DB: db, Queries: models.New(db)}
	ctx := context.Background()
	if got := h.gexTickSize(ctx, "MES"); got != 0.25 {
		t.Errorf("MES tick = %v", got)
	}
	// No instruments row (or a NULL-tick stub) falls back to 0.25 rather than
	// dropping the annotation.
	if got := h.gexTickSize(ctx, "NQ"); got != 0.25 {
		t.Errorf("missing-row fallback = %v", got)
	}
}
