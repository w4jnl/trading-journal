package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Every cross-page link carries the page it came from, filters included:
// the feed's entries, the trades page's group forms, the set sheet's group
// note, the analytics drill-downs.
func TestBackLinksKeepFilters(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := strconv.FormatInt(uid, 10)
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'a', 'motivewave_csv', 'A'), (2, ` + u + `, 'b', 'motivewave_csv', 'B')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
			(10, 1, 'MESZ6', 'MES', 'short', 1, 100, 99, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:01:00 +0000 UTC', 'closed', 5),
			(11, 2, 'MESZ6', 'MES', 'short', 1, 100, 99, '2026-10-09 14:00:01 +0000 UTC', '2026-10-09 14:01:00 +0000 UTC', 'closed', 5)`,
		`INSERT INTO journal_entries (id, user_id, entry_date, body, scope) VALUES (1, ` + u + `, '2026-10-08', 'x', 'day')`,
		`INSERT INTO journal_entry_labels (journal_entry_id, kind, label) VALUES (1, 'mistake', 'chased')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	q := models.New(conn)
	if _, err := copyset.Rebuild(context.Background(), q, uid); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	get := func(handler http.HandlerFunc, path string, hdr map[string]string, pathKey, pathVal string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if pathKey != "" {
			req.SetPathValue(pathKey, pathVal)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	// Feed with a filter: entries and the quick-add bar come back to it.
	body := strings.ToLower(get(h.JournalFeed, "/journal?mistake=chased", nil, "", "")) // html/template writes lowercase hex
	for _, want := range []string{`href="/journal/2026-10-08?back=%2fjournal%3fmistake%3dchased"`, `?back=%2fjournal%3fmistake%3dchased">+ today</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("feed missing %q: %s", want, sheetSnippet(body, "+ Today"))
		}
	}
	// Trades page: the group forms carry the filtered list; the sheet deep link is dropped.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /trades", h.TradesPage)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/trades?symbol_root=MES&view=trades&sheet=5", nil).WithContext(ctx))
	if b := rec.Body.String(); !strings.Contains(b, `name="back" value="/trades?symbol_root=MES&amp;view=trades"`) {
		t.Errorf("trades group form should come back to the filtered list: %s", sheetSnippet(b, `name="back"`))
	}
	// Set sheet: back is the page the sheet sits on (HX-Current-URL), transient params dropped.
	var sid int64
	_ = conn.QueryRow(`SELECT id FROM trade_sets`).Scan(&sid)
	hdr := map[string]string{"HX-Current-URL": "http://journal.local/trades?view=sets&symbol_root=MES&sheet=5"}
	if _, err := conn.Exec(`INSERT INTO journal_entries (id, user_id, entry_date, body, scope) VALUES (2, ` + u + `, '2026-10-09', 'g', 'group')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (2, 10), (2, 11)`); err != nil {
		t.Fatal(err)
	}
	sheet := get(h.TradeSetDetail, "/trades/sets/"+strconv.FormatInt(sid, 10), hdr, "id", strconv.FormatInt(sid, 10))
	if !strings.Contains(strings.ToLower(sheet), `href="/journal/group/2?back=%2ftrades%3fsymbol_root%3dmes%26view%3dsets"`) {
		t.Errorf("set sheet group link lost the page: %s", sheetSnippet(sheet, "group/2"))
	}
	if hxPageURL(httptest.NewRequest(http.MethodGet, "/x", nil), "/fallback") != "/fallback" {
		t.Error("no HX-Current-URL should fall back")
	}
	// Analytics drill-downs come back to the same analytics view.
	mux = http.NewServeMux()
	mux.HandleFunc("GET /analytics", h.AnalyticsPage)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/analytics?from=2026-10-01&to=2026-10-10", nil).WithContext(ctx))
	if b := rec.Body.String(); !strings.Contains(b, `href="/trades?symbol_root=MES&back=%2Fanalytics%3Ffrom%3D2026-10-01%26to%3D2026-10-10"`) {
		t.Errorf("analytics drill-down lost the view: %s", sheetSnippet(b, "symbol_root=MES"))
	}
}

func sheetSnippet(body, around string) string {
	i := strings.Index(body, around)
	if i < 0 {
		return "(not found)"
	}
	lo, hi := i-80, i+120
	if lo < 0 {
		lo = 0
	}
	if hi > len(body) {
		hi = len(body)
	}
	return body[lo:hi]
}
