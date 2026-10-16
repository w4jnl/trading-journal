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

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The pre-market plan end to end: written on the day page, shown with its
// checks against the day, nudged from the dashboard, read and amended
// through the API and MCP (today or later only).
func TestDayPlanFlow(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := strconv.FormatInt(uid, 10)
	// 9 Oct: three longs, the last after 21:00.
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'mine', 'motivewave_csv', 'A-1')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
			(10, 1, 'MESZ6', 'MES', 'long', 1, 100, 101, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:01:00 +0000 UTC', 'closed', 5),
			(11, 1, 'MESZ6', 'MES', 'long', 1, 100, 99, '2026-10-09 15:00:00 +0000 UTC', '2026-10-09 15:01:00 +0000 UTC', 'closed', -5),
			(12, 1, 'MESZ6', 'MES', 'long', 1, 100, 101, '2026-10-09 21:30:00 +0000 UTC', '2026-10-09 21:31:00 +0000 UTC', 'closed', 5)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier()}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})

	// Write the plan on the (past) day page: saved, flagged as written late.
	form := url.Values{"bias": {"long"}, "max_trades": {"2"}, "stop_time": {"21:00"}, "max_loss": {"50"}, "levels": {"7860 HVL"}, "notes": {"only the open"}, "back": {"/journal?mistake=chased"}}
	req := httptest.NewRequest(http.MethodPost, "/journal/plan/2026-10-09", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("date", "2026-10-09")
	rec := httptest.NewRecorder()
	h.DayPlanSave(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/journal/2026-10-09?back=%2Fjournal%3Fmistake%3Dchased#day-plan" {
		t.Fatalf("save -> %d %q", rec.Code, rec.Header().Get("Location"))
	}
	page := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/journal/2026-10-09", nil).WithContext(ctx)
		req.SetPathValue("date", "2026-10-09")
		rec := httptest.NewRecorder()
		h.JournalPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("day page -> %d", rec.Code)
		}
		return rec.Body.String()
	}
	body := page()
	for _, want := range []string{
		`id="day-plan"`, `<span class="num">2</span>/<span class="num">4</span> rules held`, `written after the first trade`,
		`plan-bias--long">Long</span>`, `at most <b class="num">2</b> decisions`, `<pre>7860 HVL</pre>`,
		`plan-check--broken`, `At most 2 decision(s)</span><span class="plan-check__actual">took 3`,
		`Done by 21:00</span><span class="plan-check__actual">last entry 21:30`,
		`Traded past the plan&#39;s stop time`, `More decisions than the plan allowed`,
		`<option value="long" selected>Long</option>`, `name="max_trades" min="1" step="1" inputmode="numeric" value="2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("day page missing %q", want)
		}
	}

	// API: past days are refused; today is written and merged.
	post := func(js string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/plan", strings.NewReader(js)).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.APIDayPlanSave(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := post(`{"day":"2026-10-09","bias":"short"}`); code != http.StatusBadRequest || !strings.Contains(body, "today or a future day") {
		t.Errorf("past day via API -> %d %s", code, body)
	}
	if code, body := post(`{"bias":"short","maxTrades":4,"stopTime":"9:30"}`); code != http.StatusOK || !strings.Contains(body, `"stopTime":"09:30"`) {
		t.Fatalf("today via API -> %d %s", code, body)
	}
	if code, body := post(`{"notes":"fade the open","maxTrades":0}`); code != http.StatusOK || !strings.Contains(body, `"bias":"short"`) || !strings.Contains(body, `"maxTrades":null`) || !strings.Contains(body, `"notes":"fade the open"`) {
		t.Errorf("merge via API -> %d %s", code, body)
	}
	if code, body := post(`{"bias":"sideways"}`); code != http.StatusBadRequest || !strings.Contains(body, "bias must be") {
		t.Errorf("bad bias -> %d %s", code, body)
	}
	rec = httptest.NewRecorder()
	h.APIDayPlan(rec, httptest.NewRequest(http.MethodGet, "/api/v1/plan?day=2026-10-09", nil).WithContext(ctx))
	var view struct {
		Day     string `json:"day"`
		Outcome struct {
			Held   int `json:"held"`
			Checks []struct {
				Key  string `json:"key"`
				Held bool   `json:"held"`
			} `json:"checks"`
			WrittenLate bool `json:"writtenLate"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.Day != "2026-10-09" || view.Outcome.Held != 2 || len(view.Outcome.Checks) != 4 || !view.Outcome.WrittenLate {
		t.Errorf("GET plan = %s", rec.Body.String())
	}

	// MCP: set merges for today, get reads it back.
	mreq := mcpgo.CallToolRequest{}
	mreq.Params.Arguments = map[string]any{"levels": "7900 call wall", "maxLoss": float64(250)}
	res, err := h.mcpSetDayPlan(ctx, mreq)
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, `"maxLoss":250`) || !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, `"bias":"short"`) {
		t.Fatalf("mcp set = %+v %v", res, err)
	}
	mreq.Params.Arguments = map[string]any{"day": "2026-10-09", "bias": "long"}
	if res, _ := h.mcpSetDayPlan(ctx, mreq); !res.IsError {
		t.Error("mcp set on a past day should be refused")
	}
	mreq.Params.Arguments = map[string]any{}
	res, _ = h.mcpGetDayPlan(ctx, mreq)
	if txt := res.Content[0].(mcpgo.TextContent).Text; !strings.Contains(txt, `"levels":"7900 call wall"`) || !strings.Contains(txt, `"outcome":null`) && !strings.Contains(txt, `"checks":[]`) {
		t.Errorf("mcp get today = %s", txt)
	}

	// Dashboard: today's plan as its own card, with the levels; the "write it"
	// line of the Today panel is gone once a plan exists.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard", h.DashboardPage)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil).WithContext(ctx))
	if b := rec.Body.String(); !strings.Contains(b, `id="dash-plan"`) || !strings.Contains(b, `plan-bias--short`) || !strings.Contains(b, `done by <b class="num">09:30</b>`) || !strings.Contains(b, `7900 call wall`) || strings.Contains(b, `class="dash-plan"`) {
		t.Errorf("dashboard plan card missing: %s", sheetSnippet(b, "dash-plan"))
	}

	// Delete from the page.
	req = httptest.NewRequest(http.MethodPost, "/journal/plan/2026-10-09", strings.NewReader("_action=delete")).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("date", "2026-10-09")
	rec = httptest.NewRecorder()
	h.DayPlanSave(rec, req)
	if body := page(); rec.Code != http.StatusSeeOther || strings.Contains(body, "rules held") || !strings.Contains(body, "No plan for this day.") {
		t.Errorf("delete -> %d; page still shows the plan", rec.Code)
	}
}
