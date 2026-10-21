package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Route smoke: every GET route the server mounts renders against a migrated
// database seeded from the real MotiveWave export, as the bootstrap admin.
// Core pages must answer 200 (and their live fragments must carry the
// out-of-band swap); auxiliary routes whose backend is not wired in the
// fixture (market data, GEX, AI) may answer a clean 4xx but never a 5xx or
// a panic. A guard compares the table below with cmd/server/main.go so a
// route added there without a smoke entry fails here.

type smokeRoute struct {
	pattern string       // as registered in main.go
	handler http.Handler // the handler main.go mounts
	core    bool         // must be 200 (the root must redirect to /dashboard)
	query   string       // optional query string
}

type smokeFixture struct {
	h       *Handlers
	user    *auth.User
	day     string // YYYY-MM-DD of a trading day in the export (trading tz)
	week    string // ISO week of that day, "2026-W19"
	tradeID int64
	groupID int64
	setID   int64 // a copy set from the sample (the sample's accounts copy each other)
	dayID   int64
	attID   int64
	acctID  int64  // the account of tradeID, for the position sheet route
	symbol  string // its contract
}

func newSmokeFixture(t *testing.T) *smokeFixture {
	t.Helper()
	src := filepath.Join("..", "..", "exportData", "MotivewaveExport.csv")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("sample export missing: %v", err)
	}
	// The reader pool is query_only: a GET handler that writes fails here.
	conn, rconn := dbtest.OpenWithReader(t, "bootstrap")
	q := models.New(conn)
	ctx := context.Background()
	uid := dbtest.BootstrapUserID(t, conn, "bootstrap")
	loc, _ := time.LoadLocation("America/Chicago")
	tmp := t.TempDir()
	for _, d := range []string{"processed", "failed", "inbox", "uploads", "attachments", "oi"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	svc := &importer.ImportService{
		DB: conn, Queries: q, Timezone: loc, UserID: uid,
		ProcessedDir: filepath.Join(tmp, "processed"), FailedDir: filepath.Join(tmp, "failed"),
	}
	// Copy the fixture: the importer moves its input into processed/.
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "inbox", "MotivewaveExport.csv")
	if err := os.WriteFile(in, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportFile(ctx, in); err != nil {
		t.Fatalf("import sample: %v", err)
	}
	var tradeID, acctID int64
	var symbol string
	var exit time.Time
	if err := conn.QueryRow(`SELECT id, account_id, symbol, exit_time FROM trades WHERE status = 'closed' ORDER BY exit_time DESC LIMIT 1`).Scan(&tradeID, &acctID, &symbol, &exit); err != nil {
		t.Fatalf("no closed trades after import: %v", err)
	}
	// Copy sets: the import's rebuild builds them; take one for the set routes.
	var setID int64
	if err := conn.QueryRow(`SELECT id FROM trade_sets ORDER BY id LIMIT 1`).Scan(&setID); err != nil {
		t.Fatalf("no copy set after import (the sample has copied accounts): %v", err)
	}
	local := exit.In(loc)
	day := local.Format("2006-01-02")
	y, w := local.ISOWeek()
	dayEntry, err := q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: &uid, EntryDate: time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc), Body: "# Day\n\nsmoke day note", Scope: "day",
	})
	if err != nil {
		t.Fatal(err)
	}
	groupEntry, err := q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
		UserID: &uid, EntryDate: time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc), Body: "smoke group note", Scope: "group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)`, groupEntry.ID, tradeID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "attachments", "smoke.png"), []byte("\x89PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := conn.Exec(`INSERT INTO journal_attachments (entry_id, entry_scope, file_path, mime, byte_size, user_id) VALUES (?, 'day', 'smoke.png', 'image/png', 4, ?)`, dayEntry.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	attID, _ := res.LastInsertId()
	return &smokeFixture{
		h: &Handlers{
			Templates: web.TemplatesFS, DB: conn, Queries: q, RDB: rconn, RQ: models.New(rconn), ImportSvc: svc,
			UploadsDir: filepath.Join(tmp, "uploads"), AttachmentsDir: filepath.Join(tmp, "attachments"), OIDashboardsDir: filepath.Join(tmp, "oi"),
			Timezone: loc, Version: "smoke", LiveData: livedata.NewNotifier(),
		},
		user:    &auth.User{ID: uid, Username: "bootstrap", DisplayName: "Bootstrap", IsAdmin: true},
		day:     day,
		week:    fmt.Sprintf("%d-W%02d", y, w),
		tradeID: tradeID, groupID: groupEntry.ID, setID: setID, dayID: dayEntry.ID, attID: attID,
		acctID: acctID, symbol: symbol,
	}
}

// routes mirrors the GET table in cmd/server/main.go (checked by
// TestSmokeRoutesMatchMain). Patterns use the same placeholders.
func (f *smokeFixture) routes() []smokeRoute {
	h := f.h
	hf := func(fn func(http.ResponseWriter, *http.Request)) http.Handler { return http.HandlerFunc(fn) }
	return []smokeRoute{
		{"/{$}", hf(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/dashboard", http.StatusFound) }), true, ""},
		{"/import", hf(h.ImportPage), true, ""},
		{"/trades", hf(h.TradesPage), true, ""},
		{"/trades/{id}", hf(h.TradeDetail), true, ""},
		{"/trades/sets/{id}", hf(h.TradeSetDetail), true, ""},
		{"/positions/{account}/{symbol}", hf(h.PositionSheet), true, ""},
		{"/charts/trade/{id}", hf(h.ChartTrade), false, ""},
		{"/charts/set/{id}", hf(h.ChartSet), false, ""},
		{"/charts/group/{id}", hf(h.ChartGroup), false, ""},
		{"/charts/day/{date}", hf(h.ChartDay), false, ""},
		{"/charts/week/{week}", hf(h.ChartWeek), false, ""},
		{"/accounts", hf(h.AccountsPage), true, ""},
		{"/reconcile/copy", hf(h.CopyReconcilePage), true, ""},
		{"/commissions", hf(h.CommissionsPage), true, ""},
		{"/library", hf(h.LibraryPage), true, ""},
		{"/settings", hf(h.SettingsPage), true, ""},
		{"/settings/api-docs", hf(h.APIDocsPage), true, ""},
		{"/settings/api-docs/openapi.json", hf(h.APIOpenAPI), true, ""},
		{"/journal", hf(h.JournalFeed), true, ""},
		{"/journal/export.md", hf(h.JournalExportMarkdown), true, ""},
		{"/journal/export.pdf", hf(h.JournalExportPDF), true, ""},
		{"/journal/templates", hf(h.JournalTemplatesList), true, "?scope=day"},
		{"/journal/recent", hf(h.JournalRecentList), true, "?scope=day"},
		{"/journal/attachments/{id}", hf(h.JournalAttachmentServe), true, ""},
		{"/journal/lessons", hf(h.LessonsPage), true, ""},
		{"/journal/week/{week}", hf(h.WeekJournalPage), true, ""},
		{"/journal/trade/{id}", hf(h.JournalTradePage), true, ""},
		{"/journal/group/{id}", hf(h.GroupJournalPage), true, ""},
		{"/journal/{date}", hf(h.JournalPage), true, ""},
		{"/ai/day/{date}/status", hf(h.AIDayStatus), false, ""},
		{"/ai/review/{id}", hf(h.AIReviewStatus), false, ""},
		{"/ai/tasks/status", hf(h.AITaskStatus), false, ""},
		{"/calendar", hf(h.CalendarPage), true, ""},
		{"/dashboard", hf(h.DashboardPage), true, ""},
		{"/analytics", hf(h.AnalyticsPage), true, ""},
		{"/analytics/lab", hf(h.LabPage), true, ""},
		{"/digest", hf(h.DigestPage), true, ""},
		{"/digest.md", hf(h.DigestMarkdown), false, ""},
		{"/data/version", hf(h.DataVersion), true, ""},
		{"/levels", hf(h.LevelsPage), true, ""},
		{"/levels/data/{market}", hf(h.LevelsData), false, ""},
		{"/levels/version/{market}", hf(h.LevelsVersion), false, ""},
		{"/levels/{market}", hf(h.LevelsPage), true, ""},
		{"/oi", hf(h.OIDashboardPage), true, ""},
		{"/oi/{date}", hf(h.OIDashboardPage), false, ""},
		{"/oi/file/{name}", hf(h.OIDashboardFile), false, ""},
		{"/gex", hf(h.GEXPage), false, ""},
		{"/gex/exposure", hf(h.GEXExposurePage), false, ""},
		{"/gex/exposure/{market}", hf(h.GEXExposurePage), false, ""},
		{"/gex/exposure/data/{market}", hf(h.GEXExposureData), false, ""},
		{"/gex/data/{market}", hf(h.GEXData), false, ""},
		{"/gex/{market}", hf(h.GEXPage), false, ""},
		{"/admin/users", hf(h.AdminUsersPage), true, ""},
		{"/admin/transactions", hf(h.AdminTransactionsPage), true, ""},
		{"/admin/market-data", hf(h.AdminMarketDataPage), true, ""},
	}
}

// smokeSkip are GET patterns main.go mounts that the fixture cannot drive:
// static files, the health probe closure, OAuth consent (needs the server).
// smokeSkip: not pages. /api/v1/events is the key-authenticated SSE stream
// mounted outside the write timeout.
var smokeSkip = map[string]bool{"/healthz": true, "/static/": true, "/oauth/authorize": true, "/api/v1/events": true}

func (f *smokeFixture) fill(pattern string) string {
	p := strings.NewReplacer(
		"{date}", f.day, "{week}", f.week, "{market}", "ES", "{name}", "dashboard-2026-01-01.html",
		"{account}", fmt.Sprint(f.acctID), "{symbol}", f.symbol,
	).Replace(pattern)
	switch {
	case strings.HasPrefix(pattern, "/journal/group/"), strings.HasPrefix(pattern, "/charts/group/"):
		p = strings.Replace(p, "{id}", fmt.Sprint(f.groupID), 1)
	case strings.HasPrefix(pattern, "/journal/attachments/"):
		p = strings.Replace(p, "{id}", fmt.Sprint(f.attID), 1)
	case strings.HasPrefix(pattern, "/trades/sets/"), strings.HasPrefix(pattern, "/charts/set/"):
		p = strings.Replace(p, "{id}", fmt.Sprint(f.setID), 1)
	default:
		p = strings.Replace(p, "{id}", fmt.Sprint(f.tradeID), 1)
	}
	return strings.TrimSuffix(p, "{$}")
}

func TestSmokeEveryGETRoute(t *testing.T) {
	f := newSmokeFixture(t)
	mux := http.NewServeMux()
	routes := f.routes()
	for _, rt := range routes {
		mux.Handle("GET "+rt.pattern, rt.handler)
	}
	get := func(target string) *httptest.ResponseRecorder {
		ctx := WithNonce(auth.WithUser(context.Background(), f.user), "smoke-nonce")
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, rt := range routes {
		target := f.fill(rt.pattern) + rt.query
		if target == "" {
			target = "/"
		}
		rec := get(target)
		switch {
		case rec.Code >= 500:
			t.Errorf("GET %s -> %d: %s", target, rec.Code, strings.TrimSpace(rec.Body.String()))
		case rt.pattern == "/{$}":
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/dashboard" {
				t.Errorf("GET / -> %d %q, want 302 /dashboard", rec.Code, rec.Header().Get("Location"))
			}
		case rt.core && rec.Code != http.StatusOK:
			t.Errorf("GET %s -> %d, want 200", target, rec.Code)
		}
		if rec.Code == http.StatusOK && strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			for _, bad := range unnoncedScripts(rec.Body.String(), "smoke-nonce") {
				t.Errorf("GET %s: inline script without the nonce (CSP would block it): %s", target, bad)
			}
			for _, bad := range unversionedScripts(rec.Body.String()) {
				t.Errorf("GET %s: static script without {{assetVer}} (a browser keeps the old copy for a year): %s", target, bad)
			}
		}
		t.Logf("GET %-40s %d", target, rec.Code)
	}
	// Live pages: the fragment form carries the out-of-band swap bundle.
	for _, p := range []string{"/dashboard", "/calendar", "/trades", "/analytics", "/journal/" + f.day} {
		rec := get(p + "?fragment=1")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hx-swap-oob") {
			t.Errorf("GET %s?fragment=1 -> %d, oob=%v", p, rec.Code, strings.Contains(rec.Body.String(), "hx-swap-oob"))
		}
	}
}

var scriptTagRe = regexp.MustCompile(`<script\b[^>]*>`)

// unnoncedScripts returns the <script> tags on a page that the Content
// Security Policy would refuse: not a same-origin file (src=), not a JSON
// data island, and not carrying this response's nonce.
func unnoncedScripts(body, nonce string) []string {
	var bad []string
	for _, tag := range scriptTagRe.FindAllString(body, -1) {
		switch {
		case strings.Contains(tag, " src="):
		case strings.Contains(tag, `type="application/json"`):
		case strings.Contains(tag, `nonce="`+nonce+`"`):
		default:
			bad = append(bad, tag)
		}
	}
	return bad
}

var staticScriptRe = regexp.MustCompile(`<script\b[^>]*\bsrc="(/static/[^"]*)"`)

// unversionedScripts returns the same-origin <script src> tags whose URL lacks
// the ?v= query the assetVer template func appends. Release builds serve
// /static immutable for a year, so a script referenced by its bare path keeps
// running in the trader's browser long after the binary changed it (the
// Levels ladder buttons were dead for every browser that had cached the
// previous levels.js).
func unversionedScripts(body string) []string {
	var bad []string
	for _, m := range staticScriptRe.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(m[1], "?v=") {
			bad = append(bad, m[0])
		}
	}
	return bad
}

// Drift guard: the GET patterns mounted in cmd/server/main.go must equal the
// smoke table plus the documented skips, in both directions.
func TestSmokeRoutesMatchMain(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "server", "main.go"))
	if err != nil {
		t.Skipf("main.go not readable: %v", err)
	}
	re := regexp.MustCompile(`"GET (/[^"]*)"`)
	inMain := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		inMain[m[1]] = true
	}
	f := &smokeFixture{h: &Handlers{}}
	inTable := map[string]bool{}
	for _, rt := range f.routes() {
		inTable[rt.pattern] = true
	}
	var missing, stale []string
	for p := range inMain {
		if !inTable[p] && !smokeSkip[p] {
			missing = append(missing, p)
		}
	}
	for p := range inTable {
		if !inMain[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("GET routes in main.go without a smoke entry (add to routes() or smokeSkip): %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("smoke entries no longer mounted in main.go: %v", stale)
	}
}
