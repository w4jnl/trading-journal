package handlers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/secretbox"
	"github.com/w4jnl/trading-journal/web"
)

// strategyFixture: 12 ES trades in the first hour (Chicago), 8 winners of
// +2 pts and 4 losers of -3 pts, the first 6 tagged "ORB" (5 wins, 1 loss).
func strategyFixture(t *testing.T, reply string) (*Handlers, context.Context, int64, *fakeProvider) {
	t.Helper()
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := itoa64(uid)
	if _, err := conn.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'mine', 'motivewave_csv', 'A-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO strategies (id, user_id, name, body) VALUES (5, ` + u + `, 'ORB', '## Setup\n\nBreak of the opening range. Target 2R.')`); err != nil {
		t.Fatal(err)
	}
	chicago, _ := time.LoadLocation("America/Chicago")
	day := time.Now().In(chicago).AddDate(0, 0, -20)
	for i := 0; i < 12; i++ {
		entry := time.Date(day.Year(), day.Month(), day.Day()+i, 8, 45, 0, 0, chicago)
		exit := entry.Add(3 * time.Minute)
		win := i%3 != 2 // 8 wins, 4 losses
		exitPx, net, mae, mfe := 7002.0, 100.0, 50.0, 150.0
		if !win {
			exitPx, net, mae, mfe = 6997.0, -150.0, 150.0, 25.0
		}
		if _, err := conn.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl, mae_pnl, mfe_pnl)
			VALUES (?, 1, 'ESZ6', 'ES', 'long', 1, 7000, ?, ?, ?, 'closed', ?, ?, ?)`, 100+i, exitPx, entry.UTC(), exit.UTC(), net, mae, mfe); err != nil {
			t.Fatal(err)
		}
		if i < 6 {
			if _, err := conn.Exec(`INSERT INTO journal_entries (user_id, trade_id, entry_date, body, scope, strategy_id) VALUES (?, ?, ?, ?, 'trade', 5)`,
				uid, 100+i, entry.Format("2006-01-02"), fmt.Sprintf("Took the ORB break %d, waited for the retest.\n\n---\n\n### AI review — x\n\nmachine words\n\n<!-- tj-ai:v1 -->", i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	q := models.New(conn)
	box, _ := secretbox.New(bytes.Repeat([]byte{0x44}, 32))
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier(),
		AISecrets: box, AIJobs: ai.NewJobManager(nil), AIReviews: ai.NewReviews(nil, 2), AITasks: ai.NewTasks(nil, 1)}
	fp := newFakeProvider(t, "sk-strategy-key-01", reply)
	withPresetURL(t, "openai", fp.URL)
	enc, nonce, _ := box.Seal("sk-strategy-key-01")
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	if err := q.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{UserID: uid, Provider: "openai", Model: "gpt-x", ApiKeyEnc: enc, ApiKeyNonce: nonce}); err != nil {
		t.Fatal(err)
	}
	return h, ctx, uid, fp
}

func waitTasks(t *testing.T, h *Handlers) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.AITasks.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func libPost(ctx context.Context, fn http.HandlerFunc, path, id string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

func libPage(t *testing.T, h *Handlers, ctx context.Context) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/library?tab=strategies", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.LibraryPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("library = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestMeasureDecisionsAndTraderWords(t *testing.T) {
	h, ctx, _, _ := strategyFixture(t, "")
	all, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := h.measureDecisions(ctx, all, time.UTC)
	if ev.Decisions != 12 || ev.Wins != 8 || ev.Losses != 4 || ev.WinRate != 0.667 || ev.Net != 200 || ev.Long != 12 {
		t.Errorf("counts = %+v", ev)
	}
	// ES: $50 a point. Winners sat 1 pt of heat and saw 3 pts; losers 3 and 0.5.
	if *ev.WinnerHeat.P50 != 1 || *ev.WinnerBest.P50 != 3 || *ev.LoserHeat.P80 != 3 || *ev.LoserBest.P50 != 0.5 {
		t.Errorf("excursion = %+v %+v %+v %+v", ev.WinnerHeat, ev.WinnerBest, ev.LoserHeat, ev.LoserBest)
	}
	if *ev.HoldMinutes.P50 != 3 || ev.ExitKept.P50 == nil {
		t.Errorf("hold %v kept %v", ev.HoldMinutes, ev.ExitKept)
	}
	if got := traderWords("Took the ORB break.\n\n---\n\n### AI review — x\n\nmachine words\n\n<!-- tj-ai:v1 -->", 200); got != "Took the ORB break." {
		t.Errorf("traderWords = %q", got)
	}
}

const draftReply = "```json\n" + `{"playbook": "## Setup\n\nFirst-hour ORB long.\n\n## Stop\n\n3 pts.", "checklist": ["Entry after the retest", "Stop 3 pts or less", " "], "statedVsActual": "You wrote 2R; your winners take 2 pts on 3 of risk."}` + "\n```"

// Draft from my trades: the model gets the tagged decisions' evidence and the
// trader's own words; the draft waits in the Library until applied.
func TestStrategyDraftApply(t *testing.T) {
	h, ctx, _, fp := strategyFixture(t, draftReply)
	rec := libPost(ctx, h.StrategyDraftStart, "/library/strategies/5/draft", "5", nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "flash=drafting") {
		t.Fatalf("start = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	waitTasks(t, h)
	if !strings.Contains(fp.user, `"decisions": 6`) || !strings.Contains(fp.user, "Took the ORB break 5") || strings.Contains(fp.user, "machine words") ||
		!strings.Contains(fp.user, "Target 2R") || !strings.Contains(fp.system, "## Stop") {
		t.Errorf("the model got the wrong material: %.400q", fp.user)
	}
	page := libPage(t, h, ctx)
	for _, want := range []string{"AI draft", "First-hour ORB long.", "Entry after the retest", "Stated vs actual", "You wrote 2R", "6 decisions", "Use this draft"} {
		if !strings.Contains(page, want) {
			t.Errorf("library lacks %q", want)
		}
	}
	rec = libPost(ctx, h.StrategyDraftApply, "/library/strategies/5/draft/apply", "5", nil)
	if !strings.Contains(rec.Header().Get("Location"), "flash=draft-applied") {
		t.Fatalf("apply = %q", rec.Header().Get("Location"))
	}
	st, _ := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: 5, UserID: userID(ctx)})
	lists, _ := h.journalSvc().ChecklistsForUser(ctx, userID(ctx))
	if st.Name != "ORB" || !strings.Contains(st.Body, "First-hour ORB long.") || len(lists[5]) != 2 || lists[5][1].Text != "Stop 3 pts or less" {
		t.Errorf("applied: %q %v", st.Body, lists[5])
	}
	if strings.Contains(libPage(t, h, ctx), "AI draft") {
		t.Error("the applied draft still shows")
	}
}

// A strategy without tagged trades: the failure shows once on the Library.
func TestStrategyDraftWithoutTrades(t *testing.T) {
	h, ctx, uid, _ := strategyFixture(t, draftReply)
	if _, err := h.DB.Exec(`INSERT INTO strategies (id, user_id, name, body) VALUES (6, ?, 'Empty', '')`, uid); err != nil {
		t.Fatal(err)
	}
	libPost(ctx, h.StrategyDraftStart, "/library/strategies/6/draft", "6", nil)
	waitTasks(t, h)
	if page := libPage(t, h, ctx); !strings.Contains(page, "Drafting failed: no trade is tagged with this strategy") {
		t.Error("failure not shown")
	}
	if strings.Contains(libPage(t, h, ctx), "Drafting failed") {
		t.Error("failure shown twice")
	}
}

// Find strategies: the untagged decisions form one group (ES, first hour,
// scalp); the model's reading comes back by id; a group becomes a strategy.
func TestStrategyDiscovery(t *testing.T) {
	reply := `{"groups":[{"id":"g1","name":"First-hour ES scalp","summary":"Quick longs off the open.","verdict":"keep","reason":"Wins 3 of 6.","rules":["Only in the first hour","Out within 5 minutes"]}]}`
	h, ctx, _, fp := strategyFixture(t, reply)
	// Make the 6 untagged decisions 10+ with more first-hour scalps.
	chicago, _ := time.LoadLocation("America/Chicago")
	day := time.Now().In(chicago).AddDate(0, 0, -5)
	for i := 0; i < 5; i++ {
		entry := time.Date(day.Year(), day.Month(), day.Day(), 8, 40+i*3, 0, 0, chicago)
		if _, err := h.DB.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl)
			VALUES (?, 1, 'MESZ6', 'MES', 'short', 1, 7000, 6999, ?, ?, 'closed', 5)`, 200+i, entry.UTC(), entry.Add(2*time.Minute).UTC()); err != nil {
			t.Fatal(err)
		}
	}
	libPost(ctx, h.StrategyDiscoverStart, "/library/strategies/discover", "", nil)
	waitTasks(t, h)
	if !strings.Contains(fp.user, `"session": "first hour"`) || !strings.Contains(fp.user, `"hold": "scalp (under 5 min)"`) || !strings.Contains(fp.user, `"decisions": 11,`) {
		t.Errorf("groups sent: %.300q", fp.user)
	}
	page := libPage(t, h, ctx)
	for _, want := range []string{"Found in your untagged trades", "First-hour ES scalp", "keep", "Out within 5 minutes", "Create strategy"} {
		if !strings.Contains(page, want) {
			t.Errorf("library lacks %q", want)
		}
	}
	rec := libPost(ctx, h.StrategyDiscoverCreate, "/library/strategies/discover/create", "", url.Values{"id": {"g1"}})
	if !strings.Contains(rec.Header().Get("Location"), "flash=strategy-found") {
		t.Fatalf("create = %q", rec.Header().Get("Location"))
	}
	var body string
	if err := h.DB.QueryRow(`SELECT body FROM strategies WHERE name = 'First-hour ES scalp'`).Scan(&body); err != nil || !strings.Contains(body, "Quick longs off the open.") || !strings.Contains(body, "decisions") {
		t.Errorf("created strategy body = %q (%v)", body, err)
	}
	if !strings.Contains(libPage(t, h, ctx), "strategy created ✓") {
		t.Error("the group is not marked created")
	}
}
