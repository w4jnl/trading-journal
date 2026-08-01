package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/models"

	_ "modernc.org/sqlite"
)

// gexTestDB builds the minimal schema the annotator touches: journal_entries
// (full column set - GetJournalEntryByID is SELECT *) and instruments.
func gexTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gex.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE journal_entries (
			id INTEGER PRIMARY KEY, trade_id INTEGER, account_id INTEGER,
			entry_date DATE NOT NULL, body TEXT NOT NULL, mood INTEGER,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			tag TEXT, scope TEXT NOT NULL DEFAULT 'trade',
			user_id INTEGER, strategy_id INTEGER)`,
		`CREATE TABLE instruments (
			symbol_root TEXT PRIMARY KEY, exchange TEXT, tick_size REAL,
			tick_value REAL, multiplier REAL, currency TEXT NOT NULL DEFAULT 'USD',
			needs_setup INTEGER NOT NULL DEFAULT 0, asset_class TEXT, size_class TEXT)`,
		`INSERT INTO instruments (symbol_root, tick_size) VALUES ('MES', 0.25)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
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

// gexFakeEngine serves a realistic ES snapshot with a configurable as_of and
// counts requests.
func gexFakeEngine(asOf string, calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{
			"market": "ES", "as_of": %q, "spot_index": 7494.55,
			"futures_conversion": {"ratio": 1.0038, "source": "mw_marks", "paired_at": "2026-07-31T19:34:39Z"},
			"regime": {"state": "positive", "net_gex_at_spot": 7.4e10, "percentile_20d": null},
			"flip": {"index": 7437.55, "futures": 7465.788539891167},
			"levels": [
				{"type": "call_wall", "index": 7530.0, "futures": 7558.584773164092, "gex": 1.08e10, "bucket": "all"},
				{"type": "put_wall", "index": 7300.0, "futures": 7327.711665882853, "gex": -3.9e9, "bucket": "all"}
			],
			"expected_move_1d": 9.7
		}`, asOf)
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
	if !strings.HasPrefix(body, "---\n\n_Gamma context (GEX)_") {
		t.Errorf("gamma block did not seed-replace the placeholder:\n%s", body)
	}
	if strings.Contains(body, "_AI review_") {
		t.Errorf("stale placeholder left in body:\n%s", body)
	}
	for _, want := range []string{
		"entry 15:52 ET", "exit 15:58 ET",
		"positive (P—)",
		"flip 7465.79", "above (+177 t)", // (7510-7465.79)/0.25
		"call wall 7558.58", "-194 t", // entry moment
		"put wall 7327.71", "+729 t",
		"market=ES", "regime=positive", "pctl=null", "flip_side=above",
		"dist_flip_ticks=+176.8", "dist_cw_ticks=-194.3", "dist_pw_ticks=+729.2",
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
