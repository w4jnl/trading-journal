package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Playbook checklist end to end: saved with the strategy in the Library,
// offered on the trade editor for the selected strategy, ticked on save,
// scored in the read view, carried by the API, and measured in analytics.
func TestPlaybookChecklistFlow(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := strconv.FormatInt(uid, 10)
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'mine', 'motivewave_csv', 'A-1')`,
		`INSERT INTO strategies (id, user_id, name, body) VALUES (5, ` + u + `, 'ORB', 'opening range'), (6, ` + u + `, 'VWAP', 'bounce')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, gross_pnl, commission, net_pnl) VALUES
			(10, 1, 'ESZ6', 'ES', 'long', 1, 100, 102, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:05:00 +0000 UTC', 'closed', 100, 4, 96),
			(11, 1, 'ESZ6', 'ES', 'long', 1, 100, 99, '2026-10-09 15:00:00 +0000 UTC', '2026-10-09 15:05:00 +0000 UTC', 'closed', -50, 4, -54)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier()}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	post := func(handler http.HandlerFunc, path string, form url.Values, pathKey, pathVal string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if pathKey != "" {
			req.SetPathValue(pathKey, pathVal)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	// Library: the checklist is saved with the strategy.
	if rec := post(h.StrategyUpdate, "/library/strategies/5", url.Values{"name": {"ORB"}, "body": {"opening range"}, "checklist": {"Entry at the level\nStop before entry\nFlat by 15:58"}}, "id", "5"); rec.Code != http.StatusSeeOther {
		t.Fatalf("strategy update -> %d: %s", rec.Code, rec.Body.String())
	}
	items, _ := h.Queries.ListChecklistItems(ctx, 5)
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	rec := httptest.NewRecorder()
	h.LibraryPage(rec, httptest.NewRequest(http.MethodGet, "/library?tab=strategies", nil).WithContext(ctx))
	if body := rec.Body.String(); !strings.Contains(body, `name="checklist"`) || !strings.Contains(body, "Entry at the level\nStop before entry\nFlat by 15:58</textarea>") || !strings.Contains(body, `<li>Flat by 15:58</li>`) {
		t.Error("library page should show the checklist text and preview")
	}

	// Trade page: the ORB block hidden until ORB is chosen (no strategy yet).
	page := func(id string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/journal/trade/"+id, nil).WithContext(ctx)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.JournalTradePage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("trade page -> %d", rec.Code)
		}
		return rec.Body.String()
	}
	body := page("10")
	if !strings.Contains(body, `data-checklist-for="5" hidden>`) || !strings.Contains(body, `name="check" value="`+strconv.FormatInt(items[1].ID, 10)+`"><span>Stop before entry</span>`) {
		t.Error("trade editor should carry the ORB block, hidden while no strategy is selected")
	}
	// Save with ORB and two of three rules ticked.
	form := url.Values{"body": {"took it"}, "strategy_id": {"5"}, "check": {strconv.FormatInt(items[0].ID, 10), strconv.FormatInt(items[2].ID, 10)}}
	if rec := post(h.TradeJournalSave, "/trades/10/journal", form, "id", "10"); rec.Code != http.StatusSeeOther {
		t.Fatalf("save -> %d: %s", rec.Code, rec.Body.String())
	}
	body = page("10")
	for _, want := range []string{`Playbook 2/3</span>`, `checklist-pill--miss`, `<span class="label">Missed</span> Stop before entry</div>`, `data-checklist-for="5">`, `value="` + strconv.FormatInt(items[0].ID, 10) + `" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("trade page after save missing %q", want)
		}
	}
	// Second trade: all rules ticked.
	form = url.Values{"body": {"clean"}, "strategy_id": {"5"}, "check": {strconv.FormatInt(items[0].ID, 10), strconv.FormatInt(items[1].ID, 10), strconv.FormatInt(items[2].ID, 10)}}
	if rec := post(h.TradeJournalSave, "/trades/11/journal", form, "id", "11"); rec.Code != http.StatusSeeOther {
		t.Fatalf("save 11 -> %d", rec.Code)
	}
	if body := page("11"); !strings.Contains(body, `Playbook 3/3</span>`) || !strings.Contains(body, `checklist-pill--ok`) || strings.Contains(body, `class="label">Missed`) {
		t.Error("complete checklist should read 3/3 with no misses")
	}

	// API detail carries the checklist.
	e, _ := h.Queries.GetJournalEntryByTradeID(ctx, ptrInt64(10))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/journal/"+strconv.FormatInt(e.ID, 10), nil).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(e.ID, 10))
	rec = httptest.NewRecorder()
	h.APIJournalDetail(rec, req)
	var detail struct {
		Checklist struct {
			StrategyID int64 `json:"strategyId"`
			Done       int   `json:"done"`
			Total      int   `json:"total"`
			Items      []struct {
				Text    string `json:"text"`
				Checked bool   `json:"checked"`
			} `json:"items"`
		} `json:"checklist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("detail -> %d: %s", rec.Code, rec.Body.String())
	}
	if detail.Checklist.StrategyID != 5 || detail.Checklist.Done != 2 || detail.Checklist.Total != 3 || detail.Checklist.Items[1].Checked || !detail.Checklist.Items[0].Checked {
		t.Errorf("api checklist = %+v", detail.Checklist)
	}

	// Analytics: one strategy row, 2 trades, 83% ticked on average, one complete.
	rec = httptest.NewRecorder()
	h.APIAnalyticsSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/summary?from=2026-10-01&to=2026-10-10", nil).WithContext(ctx))
	var out analyticsSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Compliance) != 1 || out.Compliance[0].Strategy != "ORB" || out.Compliance[0].Trades != 2 || out.Compliance[0].Complete != 1 || out.Compliance[0].CompleteNet != -54 || out.Compliance[0].IncompleteNet != 96 || out.Compliance[0].AvgPct < 0.83 || out.Compliance[0].AvgPct > 0.84 {
		t.Errorf("compliance = %+v", out.Compliance)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /analytics", h.AnalyticsPage)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/analytics?from=2026-10-01&to=2026-10-10", nil).WithContext(ctx))
	if body := rec.Body.String(); !strings.Contains(body, "Playbook compliance") || !strings.Contains(body, "<code>ORB</code>") || !strings.Contains(body, ">83%<") {
		t.Error("analytics page should show the compliance table")
	}
}

func ptrInt64(v int64) *int64 { return &v }
