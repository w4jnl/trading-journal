package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Two accounts copy one MES short (a set), plus a lone long on one of them.
func setsFixture(t *testing.T) (*Handlers, context.Context, int64) {
	t.Helper()
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := strconv.FormatInt(uid, 10)
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'alpha', 'motivewave_csv', 'A'), (2, ` + u + `, 'bravo', 'motivewave_csv', 'B')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, gross_pnl, commission, net_pnl, planned_risk) VALUES
			(10, 1, 'MESZ6', 'MES', 'short', 2, 7863.25, 7862.25, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:01:00 +0000 UTC', 'closed', 10, 2, 8, 100),
			(11, 2, 'MESZ6', 'MES', 'short', 1, 7863.75, 7862.25, '2026-10-09 14:00:01 +0000 UTC', '2026-10-09 14:01:05 +0000 UTC', 'closed', 7.5, 1, 6.5, NULL),
			(12, 1, 'MESZ6', 'MES', 'long', 1, 7860, 7861, '2026-10-09 15:00:00 +0000 UTC', '2026-10-09 15:01:00 +0000 UTC', 'closed', 5, 1, 4, NULL)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	q := models.New(conn)
	if n, err := copyset.Rebuild(context.Background(), q, uid); err != nil || n != 1 {
		t.Fatalf("rebuild sets = %d, %v", n, err)
	}
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier()}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	return h, ctx, uid
}

func TestTradesSetsView(t *testing.T) {
	h, ctx, uid := setsFixture(t)
	f, _ := h.parseTradeFilters(httptest.NewRequest(http.MethodGet, "/trades", nil).WithContext(ctx))
	units, err := listTradeSetsFiltered(ctx, h.DB, f)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := countTradeSetsFiltered(ctx, h.DB, f); n != 2 || len(units) != 2 {
		t.Fatalf("units = %d (count %d), want 2", len(units), n)
	}
	solo, set := units[0], units[1] // newest entry first: the 15:00 long, then the 14:00 set
	if solo.IsSet() || solo.Key != "12" || len(solo.Members) != 1 {
		t.Errorf("first unit should be the lone trade: %+v", solo)
	}
	if !set.IsSet() || len(set.Members) != 2 || set.Accounts != 2 || set.Qty != 3 || set.EntryAvg != 7863.5 || set.Spread() != 0.5 ||
		set.NetPnl == nil || *set.NetPnl != 14.5 || set.PlannedRisk == nil || *set.PlannedRisk != 100 || set.Symbol != "MESZ6 ×2" {
		t.Errorf("set unit = %+v (net %v risk %v)", set, set.NetPnl, set.PlannedRisk)
	}
	if r := set.RMultiple(); r == nil || *r != 0.145 {
		t.Errorf("set R = %v, want 14.5/100", r)
	}
	// The account filter narrows the members: the set shows alpha's leg only.
	f1, _ := h.parseTradeFilters(httptest.NewRequest(http.MethodGet, "/trades?filter=acc:1", nil).WithContext(ctx))
	if units, _ := listTradeSetsFiltered(ctx, h.DB, f1); len(units) != 2 || len(units[1].Members) != 1 || units[1].Members[0].AccountID != 1 {
		t.Errorf("filtered units = %+v", units)
	}

	// The page: toggle, set row with its folded members, remembered choice.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /trades", h.TradesPage)
	get := func(path string) string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	body := get("/trades?view=sets")
	sid := strconv.FormatInt(*set.SetID, 10)
	for _, want := range []string{
		`trades-view-seg`, `class="active" title="One row per decision; copy sets`,
		`data-trade-id="set:` + sid + `"`, `hx-get="/trades/sets/` + sid + `"`, `data-set-toggle="` + sid + `"`,
		`data-member-of="` + sid + `"`, `set ×2</span>`, `data-trade-id="12"`, `hx-get="/trades/12"`,
		`2</span> decisions`, `3</span> trades in this filter`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sets page missing %q", want)
		}
	}
	if v, err := h.Queries.GetViewPref(ctx, models.GetViewPrefParams{UserID: uid, Page: "trades.view"}); err != nil || v != "sets" {
		t.Errorf("view pref = %q, %v; want sets", v, err)
	}
	if body := get("/trades"); !strings.Contains(body, `data-trade-id="set:`+sid+`"`) {
		t.Error("remembered sets view not applied without ?view")
	}
	if body := get("/trades?view=trades"); strings.Contains(body, `data-trade-id="set:`) || !strings.Contains(body, `data-trade-id="10"`) {
		t.Error("?view=trades should list every trade")
	}

	// The set sheet: both accounts, the fill comparison, the journal action.
	req := httptest.NewRequest(http.MethodGet, "/trades/sets/"+sid, nil).WithContext(ctx)
	req.SetPathValue("id", sid)
	rec := httptest.NewRecorder()
	h.TradeSetDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set sheet -> %d: %s", rec.Code, rec.Body.String())
	}
	sheet := rec.Body.String()
	for _, want := range []string{
		`set · 2 accounts`, `<td>alpha`, `<td>bravo`, `Entry spread`, `2 ticks`, // 0.5 on a 0.25 tick
		`Best fill`, `bravo <span class="pnl-pos"`, `Worst fill`, `alpha <span class="pnl-neg"`,
		`hx-post="/trades/sets/` + sid + `/journal"`, `data-sheet-title="MES · set ×2"`,
	} {
		if !strings.Contains(sheet, want) {
			t.Errorf("set sheet missing %q", want)
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/trades/sets/999", nil).WithContext(ctx)
	req.SetPathValue("id", "999")
	rec = httptest.NewRecorder()
	h.TradeSetDetail(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown set -> %d, want 404", rec.Code)
	}

	// Journal this set: creates one group note over both trades, then reuses it.
	post := func() string {
		req := httptest.NewRequest(http.MethodPost, "/trades/sets/"+sid+"/journal", nil).WithContext(ctx)
		req.Header.Set("HX-Request", "true")
		req.SetPathValue("id", sid)
		rec := httptest.NewRecorder()
		h.TradeSetJournal(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("journal set -> %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Header().Get("HX-Redirect")
	}
	first := post()
	if !strings.HasPrefix(first, "/journal/group/") {
		t.Fatalf("redirect = %q", first)
	}
	var linked int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entry_trades jet JOIN journal_entries je ON je.id = jet.journal_entry_id WHERE je.scope = 'group'`).Scan(&linked)
	if linked != 2 {
		t.Errorf("group note links %d trades, want 2", linked)
	}
	if again := post(); again != first {
		t.Errorf("second journal action should reuse the note: %q vs %q", again, first)
	}
	req = httptest.NewRequest(http.MethodGet, "/trades/sets/"+sid, nil).WithContext(ctx)
	req.SetPathValue("id", sid)
	rec = httptest.NewRecorder()
	h.TradeSetDetail(rec, req)
	if !strings.Contains(rec.Body.String(), "Open the group note") {
		t.Error("sheet should offer the existing group note")
	}
}

func TestCollapseSetsAndSummaryUnit(t *testing.T) {
	h, ctx, _ := setsFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/analytics/summary?from=2026-10-01&to=2026-10-10&unit=sets", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.APIAnalyticsSummary(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary -> %d: %s", rec.Code, rec.Body.String())
	}
	var out analyticsSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// Two decisions (the set and the lone long) over three trades; money unchanged.
	if out.Unit != "sets" || out.Trades != 3 || out.Totals.Trades != 2 || out.Totals.Wins != 2 || out.Totals.Net != 18.5 || out.R.PlannedTrades != 1 || out.R.AvgR != 0.145 {
		t.Errorf("unit=sets summary: unit=%s trades=%d totals=%+v r=%+v", out.Unit, out.Trades, out.Totals, out.R)
	}
	rec = httptest.NewRecorder()
	h.APIAnalyticsSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/summary?from=2026-10-01&to=2026-10-10", nil).WithContext(ctx))
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Unit != "trades" || out.Totals.Trades != 3 || out.Totals.Net != 18.5 {
		t.Errorf("default unit summary: %+v", out.Totals)
	}
	// The page shows the toggle and the decisions count.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /analytics", h.AnalyticsPage)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/analytics?from=2026-10-01&to=2026-10-10&unit=sets", nil).WithContext(ctx))
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `3</span> closed trades · <span class="num">2</span> decisions`) || !strings.Contains(body, `unit=sets" class="active"`) {
		t.Errorf("analytics page unit=sets -> %d, toggle/count missing", rec.Code)
	}
	// API rows carry the set.
	rows, _ := listTradesFiltered(ctx, h.DB, tradeFilters{Page: 1})
	var inSet, alone int
	for _, r := range rows {
		a := apiTradeRowFrom(r)
		if a.SetID != nil && a.SetSize == 2 {
			inSet++
		} else if a.SetID == nil && a.SetSize == 0 {
			alone++
		}
	}
	if inSet != 2 || alone != 1 {
		t.Errorf("api rows: in set %d alone %d", inSet, alone)
	}
	if d, err := h.tradeDetail(ctx, 10); err != nil || d.SetID == nil || d.SetSize != 2 {
		t.Errorf("detail set fields: %+v %v", d.SetID, err)
	}
}

// The set row's path glyph averages the measured legs in points, sums the
// money, and says when legs are missing.
func TestSetRowPath(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tf := "1m"
	leg := func(entry, exit, mae, mfe float64, maePnl, mfePnl *float64) tradeRow {
		return tradeRow{Direction: "short", EntryPrice: entry, ExitPrice: f(exit), MaePoints: f(mae), MfePoints: f(mfe), MaePnl: maePnl, MfePnl: mfePnl, ExcursionTf: &tf}
	}
	// Two shorts out 1 pt in profit, heat 0.5, best seen 2: scale 2, heat 13%,
	// gain 50%, exit 50 + 25 = 75%.
	s := setRow{Members: []tradeRow{leg(7863.25, 7862.25, 0.5, 2, f(5), f(20)), leg(7863, 7862, 0.5, 2, f(5), f(20))}}
	p := s.Path()
	if p == nil || p.HeatPct != 13 || p.GainPct != 50 || p.ExitPct != 75 || p.Sign != "pnl-pos" || p.Title != "Heat −10.00 · best seen +40.00 · exit kept 50% (1m bars)" {
		t.Errorf("set path = %+v", p)
	}
	// An unmeasured leg is left out and named.
	s.Members = append(s.Members, tradeRow{Direction: "short", EntryPrice: 7863, ExitPrice: f(7862)})
	if p := s.Path(); p == nil || p.ExitPct != 75 || !strings.HasSuffix(p.Title, " (1m bars) · 2 of 3 legs measured") {
		t.Errorf("set path with an unmeasured leg = %+v", p)
	}
	// A measured leg without money (stub instrument) drops the tooltip to points.
	s.Members[1].MaePnl = nil
	if p := s.Path(); p == nil || !strings.HasPrefix(p.Title, "Heat 0.5 pts · best seen 2 pts") {
		t.Errorf("set path with an unpriced leg = %+v", p)
	}
	if (setRow{Members: []tradeRow{{Direction: "long", EntryPrice: 1}}}).Path() != nil {
		t.Error("a set with no measured leg should have no path")
	}
}
