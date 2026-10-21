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

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The weekly digest over two weeks: per-decision numbers against the week
// before, keep and fix from the numbers, best and worst decision, the
// coach tallied across the days, the plans' rules; the page, the markdown,
// the API, MCP and the AI week context all serve it.
func TestWeeklyDigest(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := strconv.FormatInt(uid, 10)
	// Week 41 (5-11 Oct 2026): a copy set across two accounts winning 100
	// with ORB, a loss labelled "moved the stop", and a revenge re-entry 20s
	// after it on two days. Week 40: one small win.
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'a', 'motivewave_csv', 'A'), (2, ` + u + `, 'b', 'motivewave_csv', 'B')`,
		`INSERT INTO strategies (id, user_id, name, body) VALUES (5, ` + u + `, 'ORB', 'x')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, gross_pnl, commission, net_pnl) VALUES
			(10, 1, 'MESZ6', 'MES', 'short', 1, 100, 95, '2026-10-06 14:00:00 +0000 UTC', '2026-10-06 14:05:00 +0000 UTC', 'closed', 52, 2, 50),
			(11, 2, 'MESZ6', 'MES', 'short', 1, 100, 95, '2026-10-06 14:00:01 +0000 UTC', '2026-10-06 14:05:00 +0000 UTC', 'closed', 52, 2, 50),
			(12, 1, 'MESZ6', 'MES', 'long', 1, 100, 98, '2026-10-06 15:00:00 +0000 UTC', '2026-10-06 15:01:00 +0000 UTC', 'closed', -18, 2, -20),
			(13, 1, 'MESZ6', 'MES', 'long', 1, 100, 99, '2026-10-06 15:01:20 +0000 UTC', '2026-10-06 15:02:00 +0000 UTC', 'closed', -8, 2, -10),
			(14, 1, 'MESZ6', 'MES', 'long', 1, 100, 98, '2026-10-08 15:00:00 +0000 UTC', '2026-10-08 15:01:00 +0000 UTC', 'closed', -18, 2, -20),
			(15, 1, 'MESZ6', 'MES', 'long', 1, 100, 101, '2026-10-08 15:01:20 +0000 UTC', '2026-10-08 15:02:00 +0000 UTC', 'closed', 7, 2, 5),
			(20, 1, 'MESZ6', 'MES', 'long', 1, 100, 101, '2026-09-30 14:00:00 +0000 UTC', '2026-09-30 14:01:00 +0000 UTC', 'closed', 7, 2, 5)`,
		`INSERT INTO journal_entries (id, user_id, trade_id, entry_date, body, scope, strategy_id) VALUES (1, ` + u + `, 10, '2026-10-06', 'x', 'trade', 5), (2, ` + u + `, 12, '2026-10-06', 'y', 'trade', NULL)`,
		`INSERT INTO journal_entry_labels (journal_entry_id, kind, label) VALUES (2, 'mistake', 'moved-stop')`,
		`INSERT INTO day_plans (user_id, day, bias, max_trades, stop_time, created_at, updated_at) VALUES (` + u + `, '2026-10-06', 'short', 2, '', '2026-10-06 06:00:00 +0000 UTC', '2026-10-06 06:00:00 +0000 UTC')`,
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

	monday, id, err := h.digestWeek(ctx, "2026-W41")
	if err != nil || id != "2026-W41" || monday.Format("2006-01-02") != "2026-10-05" {
		t.Fatalf("week = %v %s %v", monday, id, err)
	}
	d, err := h.buildDigest(ctx, monday, id)
	if err != nil {
		t.Fatal(err)
	}
	// 5 decisions (the copy set once) on 2 days; net 100 - 20 - 10 - 20 + 5 = 55.
	if d.This.Totals.Trades != 5 || d.TradingDays != 2 || d.This.Totals.Net != 55 || d.Prev.Totals.Trades != 1 {
		t.Fatalf("totals: this %+v prev %+v days %d", d.This.Totals, d.Prev.Totals, d.TradingDays)
	}
	kpi := map[string]digestKPI{}
	for _, k := range d.KPIs {
		kpi[k.Key] = k
	}
	if k := kpi["net"]; k.Display != "+55.00" || k.Delta != "+50.00" || k.DeltaSign != "pnl-pos" || k.PrevShown != "+5.00" {
		t.Errorf("net kpi = %+v", k)
	}
	if k := kpi["decisions"]; k.Display != "5" || k.Delta != "+4" || k.DeltaSign != "" {
		t.Errorf("decisions kpi = %+v", k)
	}
	if k := kpi["avgR"]; k.Value != nil || k.Display != "—" {
		t.Errorf("avgR without plans = %+v", k)
	}
	if d.Best == nil || d.Best.Net != 100 || d.Best.Accounts != 2 || d.Best.Strategy != "ORB" || d.Worst == nil || d.Worst.Net != -20 {
		t.Errorf("best %+v worst %+v", d.Best, d.Worst)
	}
	joined := strings.Join(d.Keep, " | ") + " || " + strings.Join(d.Fix, " | ")
	for _, want := range []string{
		"ORB carried the week: +100.00 over 1 decision(s).",
		"Moved the stop cost -20.00 over 1 decision(s).",
		"Re-entry right after a loss came up on 2 of 2 trading days.",
		"No day plan on 1 of 2 trading days.",
		"The plan's bias held on 0 of 1 planned days.", // bias and the cap both 0 of 1: the first listed is named
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("keep/fix missing %q in: %s", want, joined)
		}
	}
	var revenge *digestFinding
	for i := range d.Coach {
		if d.Coach[i].Key == "revenge-entry" {
			revenge = &d.Coach[i]
		}
	}
	if revenge == nil || revenge.Days != 2 || revenge.Count != 2 {
		t.Errorf("coach tally = %+v", d.Coach)
	}
	if d.Plan.Planned != 1 || len(d.Plan.Rules) != 2 {
		t.Errorf("plan = %+v", d.Plan)
	}

	// Page, markdown, API, MCP, AI week context.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /digest", h.DigestPage)
	mux.HandleFunc("GET /digest.md", h.DigestMarkdown)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		return rec
	}
	rec := get("/digest?week=2026-W41")
	body := rec.Body.String()
	for _, want := range []string{
		`Week 41`, `<span class="num">5</span> decisions over <span class="num">2</span> trading days`,
		`<div class="num--lg">&#43;55.00</div>`, `<span class="num pnl-pos">&#43;50.00</span>`, // html/template writes + as &#43;
		`ORB carried the week`, `Re-entry right after a loss`, `href="/digest?week=2026-W40"`,
		`href="/journal/week/2026-w41?back=%2fdigest%3fweek%3d2026-w41"`, `href="/trades?sheet=10&back=%2fdigest%3fweek%3d2026-w41"`,
		`decision cap</td><td class="tabular num pnl-neg">0 / 1`,
	} {
		if !strings.Contains(body, want) && !strings.Contains(strings.ToLower(body), want) { // html/template writes lowercase hex
			t.Errorf("page missing %q", want)
		}
	}
	if rec.Code != http.StatusOK || strings.Contains(body, "<no value>") {
		t.Errorf("page -> %d", rec.Code)
	}
	if rec := get("/digest?week=nonsense"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad week -> %d", rec.Code)
	}
	md := get("/digest.md?week=2026-W41")
	if md.Header().Get("Content-Disposition") != `attachment; filename="digest-2026-W41.md"` || !strings.Contains(md.Body.String(), "| Net P&L | +55.00 | +5.00 | +50.00 |") || !strings.Contains(md.Body.String(), "## Fix") {
		t.Errorf("markdown = %s", md.Body.String())
	}
	rec = httptest.NewRecorder()
	h.APIDigest(rec, httptest.NewRequest(http.MethodGet, "/api/v1/digest?week=2026-W41", nil).WithContext(ctx))
	var js struct {
		Week string `json:"week"`
		KPIs []struct {
			Key      string   `json:"key"`
			Value    *float64 `json:"value"`
			Previous *float64 `json:"previous"`
		} `json:"kpis"`
		Fix []string `json:"fix"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &js); err != nil || js.Week != "2026-W41" || js.KPIs[0].Key != "net" || *js.KPIs[0].Value != 55 || *js.KPIs[0].Previous != 5 || len(js.Fix) != 4 {
		t.Errorf("api digest = %s", rec.Body.String())
	}
	mreq := mcpgo.CallToolRequest{}
	mreq.Params.Arguments = map[string]any{"week": "2026-W41"}
	res, err := h.mcpGetWeeklyDigest(ctx, mreq)
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, `"week":"2026-W41"`) {
		t.Errorf("mcp digest = %+v %v", res, err)
	}
	if _, err := conn.Exec(`INSERT INTO journal_entries (id, user_id, entry_date, body, scope) VALUES (3, ` + u + `, '2026-10-05', 'week', 'week')`); err != nil {
		t.Fatal(err)
	}
	e, err := q.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: 3, UserID: &uid})
	if err != nil {
		t.Fatal(err)
	}
	blocks, _, err := h.aiEntryContext(ctx, e, time.UTC)
	var aiCtx string
	for _, b := range blocks {
		aiCtx += b.Text
	}
	if err != nil || !strings.Contains(aiCtx, `"weeklyDigest"`) || !strings.Contains(aiCtx, "ORB carried the week") {
		t.Errorf("week AI context lacks the digest: %v", err)
	}
}
