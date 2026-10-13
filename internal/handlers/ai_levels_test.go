package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/secretbox"
	"github.com/w4jnl/trading-journal/web"
)

// levelsFixture: ten earlier ES long winners (2 pts of heat, 6 pts best),
// then trade 50 (long 2 ES at 7000, 14:10) with 1m bars around it - a swing
// low of 6995 at 13:55 before the entry, a run to 7013.75 after it.
func levelsFixture(t *testing.T, reply string) (*Handlers, context.Context, *fakeProvider) {
	t.Helper()
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	if _, err := conn.Exec(`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ?, 'mine', 'motivewave_csv', 'A-1')`, uid); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		en := day.AddDate(0, 0, -i-1).Add(14 * time.Hour)
		if _, err := conn.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl, mae_pnl, mfe_pnl)
			VALUES (?, 1, 'ESZ6', 'ES', 'long', 1, 7000, 7004, ?, ?, 'closed', 200, 100, 300)`, 10+i, en, en.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	entry := day.Add(14*time.Hour + 10*time.Minute)
	if _, err := conn.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl)
		VALUES (50, 1, 'ESZ6', 'ES', 'long', 2, 7000, 7004, ?, ?, 'closed', 400)`, entry, entry.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	sym, _ := marketdata.YahooSymbol("ES")
	bar := func(at time.Time, lo, hi float64) {
		if _, err := conn.Exec(`INSERT INTO price_bars VALUES (?, '1m', ?, ?, ?, ?, ?, 0, 0)`, sym, at.Unix(), (lo+hi)/2, hi, lo, (lo+hi)/2); err != nil {
			t.Fatal(err)
		}
	}
	start := day.Add(13*time.Hour + 40*time.Minute)
	for m := 0; m < 30; m++ { // 13:40 .. 14:09: around 7000, a dip to 6995 at 13:55
		lo, hi := 6999.0, 7001.0
		if m == 15 {
			lo = 6995
		}
		bar(start.Add(time.Duration(m)*time.Minute), lo, hi)
	}
	for m := 0; m < 30; m++ { // 14:10 ..: up to 7013.75 at 14:30
		p := 7000 + float64(min(m, 20))*0.6
		hi := p + 1
		if m == 20 {
			hi = 7013.75
		}
		bar(entry.Add(time.Duration(m)*time.Minute), p-1, hi)
	}
	q := models.New(conn)
	box, _ := secretbox.New(bytes.Repeat([]byte{0x55}, 32))
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier(),
		MarketData: &marketdata.CachingSource{DB: conn, Loc: time.UTC},
		AISecrets:  box, AIJobs: ai.NewJobManager(nil), AIReviews: ai.NewReviews(nil, 2), AITasks: ai.NewTasks(nil, 1)}
	fp := newFakeProvider(t, "sk-levels-key-001", reply)
	withPresetURL(t, "openai", fp.URL)
	enc, nonce, _ := box.Seal("sk-levels-key-001")
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	if err := q.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{UserID: uid, Provider: "openai", Model: "gpt-x", ApiKeyEnc: enc, ApiKeyNonce: nonce}); err != nil {
		t.Fatal(err)
	}
	return h, ctx, fp
}

func sheetCall(ctx context.Context, fn http.HandlerFunc, method, target, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil).WithContext(ctx)
	req.Header.Set("HX-Request", "true")
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

func TestProposeLevelsForATrade(t *testing.T) {
	h, ctx, fp := levelsFixture(t, `{"stop": 6994.8, "target": 7008, "reasons": ["Below the 13:55 swing low.", "Your winners ran 6 pts."]}`)
	rec := sheetCall(ctx, h.TradeLevelsStart, http.MethodPost, "/trades/50/levels", "50")
	if !strings.Contains(rec.Body.String(), "Working out a stop and a target") || !strings.Contains(rec.Body.String(), "k=levels:trade:50&amp;then=trade:50") {
		t.Fatalf("sheet does not show the proposal being worked out: %.300q", rec.Body.String())
	}
	waitTasks(t, h)

	// The model saw candidates from before the entry only.
	var sent struct {
		Candidates []struct{ Kind, Source, Note string }
		History    struct {
			Scope string
			N     int
		}
	}
	if err := json.Unmarshal([]byte(fp.user), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.History.Scope != "ES long" || sent.History.N != 10 || len(sent.Candidates) == 0 {
		t.Errorf("sent history %+v, %d candidates", sent.History, len(sent.Candidates))
	}
	if strings.Contains(fp.user, "7013.75") {
		t.Error("a price from after the entry reached the model")
	}
	swing := false
	for _, c := range sent.Candidates {
		swing = swing || c.Kind == "stop" && strings.Contains(c.Note, "swing low 13:55")
	}
	if !swing {
		t.Errorf("no swing-low stop among %+v", sent.Candidates)
	}

	rec = sheetCall(ctx, h.AITaskStatus, http.MethodGet, "/ai/tasks/status?k=levels:trade:50&then=trade:50", "")
	body := rec.Body.String()
	if rec.Header().Get("HX-Retarget") != "#trade-sheet-content" {
		t.Errorf("status did not redraw the sheet: %v", rec.Header())
	}
	for _, want := range []string{"Proposed levels", "6994.75", "−5.25 pts", "7008", "+8.00 pts", `reward:risk <strong class="num">1.5`, "Below the 13:55 swing low.",
		"the target was reached at 14:22 (+8.00 pts, 12 min in)", "Your exit", "&#43;4.00 pts", "Use as plan", "your record: ES long, 10 earlier decisions"} {
		if !strings.Contains(body, want) {
			t.Errorf("sheet lacks %q", want)
		}
	}

	sheetCall(ctx, h.TradeLevelsApply, http.MethodPost, "/trades/50/levels/apply", "50")
	tr, _ := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: 50, UserID: userID(ctx)})
	if tr.PlannedStop == nil || *tr.PlannedStop != 6994.75 || tr.PlannedRisk == nil || *tr.PlannedRisk != 525 || tr.PlannedTarget == nil || *tr.PlannedTarget != 7008 {
		t.Errorf("plan = stop %v risk %v target %v", tr.PlannedStop, tr.PlannedRisk, tr.PlannedTarget)
	}
	if body := sheetCall(ctx, h.TradeDetail, http.MethodGet, "/trades/50", "50").Body.String(); strings.Contains(body, "Proposed levels") ||
		!strings.Contains(body, `name="target" value="7008"`) || !strings.Contains(body, "reward:risk <strong class=\"num\">1.5") {
		t.Error("after apply: the proposal should be gone and the plan row show the target and reward:risk")
	}
}

func TestProposeLevelsWrongSide(t *testing.T) {
	h, ctx, _ := levelsFixture(t, `{"stop": 7003, "target": 7008, "reasons": []}`)
	sheetCall(ctx, h.TradeLevelsStart, http.MethodPost, "/trades/50/levels", "50")
	waitTasks(t, h)
	body := sheetCall(ctx, h.TradeDetail, http.MethodGet, "/trades/50", "50").Body.String()
	if !strings.Contains(body, "Proposing levels failed: the model put the stop (7003)") {
		t.Errorf("wrong-side answer not reported: %.200q", body)
	}
}

// Use as plan on a set: every leg gets the stop (its own risk) and target.
func TestSetLevelsApply(t *testing.T) {
	h, ctx, _ := setsFixture(t)
	var sid string
	if err := h.DB.QueryRow(`SELECT CAST(id AS TEXT) FROM trade_sets LIMIT 1`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	h.AITasks = ai.NewTasks(nil, 1)
	h.AIJobs = ai.NewJobManager(nil)
	data, _ := json.Marshal(levelsData{Subject: "set:" + sid, Direction: "short", Entry: 7863.5, Stop: 7866, Target: 7858, Legs: []int64{10, 11}})
	if err := h.Queries.UpsertAIOutput(ctx, models.UpsertAIOutputParams{UserID: userID(ctx), Kind: aiKindLevels, Ref: "set:" + sid, Data: string(data)}); err != nil {
		t.Fatal(err)
	}
	if body := sheetCall(ctx, h.TradeSetDetail, http.MethodGet, "/trades/sets/"+sid, sid).Body.String(); !strings.Contains(body, "Proposed levels") {
		t.Fatal("set sheet does not show the proposal")
	}
	sheetCall(ctx, h.SetLevelsApply, http.MethodPost, "/trades/sets/"+sid+"/levels/apply", sid)
	for _, id := range []int64{10, 11} {
		tr, _ := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)})
		if tr.PlannedStop == nil || *tr.PlannedStop != 7866 || tr.PlannedTarget == nil || *tr.PlannedTarget != 7858 || tr.PlannedRisk == nil {
			t.Errorf("leg %d plan = %v %v %v", id, tr.PlannedStop, tr.PlannedTarget, tr.PlannedRisk)
		}
	}
	if body := sheetCall(ctx, h.TradeSetDetail, http.MethodGet, "/trades/sets/"+sid, sid).Body.String(); !strings.Contains(body, `name="target" value="7858"`) {
		t.Error("set plan row does not show the common target")
	}
}
