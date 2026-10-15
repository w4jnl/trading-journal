package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/secretbox"
)

// The model's answer is checked against the position: wrong-side levels
// and levels outside the bars are refused, prices snap to the tick, and
// risk and reward:risk are the app's own numbers.
func TestParseAILevels(t *testing.T) {
	tick, val := 0.25, 12.5
	es := models.Instrument{SymbolRoot: "ES", TickSize: &tick, TickValue: &val}
	good := "```json\n{\"stop\": 7240.1, \"target\": 7271, \"target2\": 7290, \"scaleOut\": 0.5, \"trail\": \"breakeven-at-1r\", \"reasons\": {\"stop\": \"below the swing\", \"target\": \"prior high\"}, \"summary\": \"ok\"}\n```"
	lv, err := parseAILevels(good, "long", 7251, 3, es, 7230, 7300)
	if err != nil {
		t.Fatal(err)
	}
	if lv.Stop != 7240 || lv.Target != 7271 || lv.Target2 == nil || *lv.Target2 != 7290 || lv.Trail != "breakeven-at-1r" || lv.ScaleOut == nil {
		t.Errorf("parsed = %+v", lv)
	}
	// |7251 - 7240| * 50 * 3 = 1650; (7271 - 7251) / 11 = 1.818
	if lv.Risk == nil || *lv.Risk != 1650 || lv.R1 == nil || *lv.R1 < 1.81 || *lv.R1 > 1.82 || lv.R2 == nil {
		t.Errorf("risk = %v r1 = %v r2 = %v", lv.Risk, lv.R1, lv.R2)
	}
	cases := []struct{ name, text, dir string }{
		{"stop above a long's entry", `{"stop": 7260, "target": 7271, "summary": ""}`, "long"},
		{"target below a long's entry", `{"stop": 7240, "target": 7245, "summary": ""}`, "long"},
		{"stop below a short's entry", `{"stop": 7240, "target": 7230, "summary": ""}`, "short"},
		{"outside the bars", `{"stop": 7000, "target": 7271, "summary": ""}`, "long"},
		{"no stop", `{"target": 7271, "summary": ""}`, "long"},
		{"prose", `I would put the stop at 7240.`, "long"},
	}
	for _, c := range cases {
		if _, err := parseAILevels(c.text, c.dir, 7251, 3, es, 7230, 7300); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	// A second target on the wrong side is dropped, not fatal; an unknown
	// trail reads as none; without bars the range check is skipped.
	lv, err = parseAILevels(`{"stop": 7240, "target": 7271, "target2": 7260, "trail": "x", "summary": "s"}`, "long", 7251, 3, es, 0, 0)
	if err != nil || lv.Target2 != nil || lv.Trail != "none" {
		t.Errorf("lenient parse = %+v (%v)", lv, err)
	}
}

// aiPositionFixture is the position fixture with a fake provider.
func aiPositionFixture(t *testing.T, reply string) (*positionFixture, *fakeProvider) {
	t.Helper()
	fx := newPositionFixture(t)
	box, _ := secretbox.New(bytes.Repeat([]byte{0x45}, 32))
	fx.h.AISecrets, fx.h.AIJobs, fx.h.AIReviews, fx.h.AITasks = box, ai.NewJobManager(nil), ai.NewReviews(nil, 2), ai.NewTasks(nil, 1)
	fp := newFakeProvider(t, "sk-levels-key-01", reply)
	withPresetURL(t, "openai", fp.URL)
	enc, nonce, _ := box.Seal("sk-levels-key-01")
	if err := fx.q.UpsertUserAISettings(fx.ctx, models.UpsertUserAISettingsParams{UserID: fx.uid, Provider: "openai", Model: "gpt-x", ApiKeyEnc: enc, ApiKeyNonce: nonce}); err != nil {
		t.Fatal(err)
	}
	return fx, fp
}

func (fx *positionFixture) post(t *testing.T, fn http.HandlerFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil).WithContext(fx.ctx)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("account", strconv.FormatInt(fx.acct.ID, 10))
	req.SetPathValue("symbol", "ESM6")
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

// The suggestion runs in the background, lands on the sheet as dashed
// levels with the app's risk and R, is written into the plan with source
// 'ai' on "Use as plan", and stays on the trade sheet after the close.
func TestPositionSuggestion(t *testing.T) {
	fx, fp := aiPositionFixture(t, `{"stop": 7241, "target": 7271, "target2": null, "scaleOut": null, "trail": "none", "reasons": {"stop": "below the opening swing low", "target": "yesterday's high"}, "summary": "Stop under the swing, target at the prior high."}`)
	ctx := fx.ctx

	if body := fx.sheet(t); !strings.Contains(body, "Suggest stop &amp; target") || strings.Contains(body, "Use as plan") {
		t.Fatalf("fresh sheet should offer the suggestion and nothing to apply")
	}
	rec := fx.post(t, fx.h.PositionSuggestStart, "/positions/x/y/suggest")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Reading the position") {
		t.Fatalf("start = %d: %s", rec.Code, rec.Body.String())
	}
	waitTasks(t, fx.h)
	fp.mu.Lock()
	user, system := fp.user, fp.system
	fp.mu.Unlock()
	if !strings.Contains(system, "OPEN RIGHT NOW") || !strings.Contains(user, `"avgEntry": 7251`) || !strings.Contains(user, `"direction": "long"`) {
		t.Errorf("prompt did not carry the position: %s", user)
	}
	if strings.Contains(user, "recordedPlan") {
		t.Errorf("no plan was recorded, none should be claimed")
	}

	body := fx.sheet(t)
	for _, want := range []string{"stop 7241", "target 7271", "2.0R", "risk 1500.00", "Stop under the swing", "Use as plan", "no bars were available"} {
		if !strings.Contains(body, want) {
			t.Errorf("sheet lacks %q", want)
		}
	}
	// Nothing written yet.
	if plans, _ := fx.q.ListOpenPositionPlansForUser(context.Background(), fx.uid); len(plans) != 0 {
		t.Fatalf("the suggestion wrote a plan: %+v", plans)
	}

	rec = fx.post(t, fx.h.PositionSuggestApply, "/positions/x/y/suggest/apply")
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d: %s", rec.Code, rec.Body.String())
	}
	plans, _ := fx.q.ListOpenPositionPlansForUser(context.Background(), fx.uid)
	if len(plans) != 1 || plans[0].Source != "ai" || plans[0].Stop == nil || *plans[0].Stop != 7241 || plans[0].Target == nil || *plans[0].Target != 7271 || plans[0].Risk == nil || *plans[0].Risk != 1500 || !strings.Contains(plans[0].Thesis, "Stop under the swing") {
		t.Fatalf("applied plan = %+v", plans)
	}
	if out := rec.Body.String(); !strings.Contains(out, "✨ ai") || !strings.Contains(out, "Suggest again") {
		t.Errorf("sheet after apply should mark the plan as from the AI")
	}

	// The position closes: the trade carries the plan and the frozen
	// suggestion next to it.
	oid := "O-9"
	if _, err := fx.q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: fx.acct.ID, BrokerExecID: "P9", OrderID: &oid, ImportMethod: "motivewave_csv",
		Symbol: "ESM6", SymbolRoot: "ES", Side: "sell", Qty: 3, Price: 7268, ExecTime: fx.t0.Add(40 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.RebuildTradesForAccountSymbol(ctx, fx.uid, fx.acct.ID, "ESM6"); err != nil {
		t.Fatal(err)
	}
	trades, _ := fx.q.ListTradesForAccount(ctx, fx.acct.ID)
	req := httptest.NewRequest(http.MethodGet, "/trades/x", nil).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(trades[0].ID, 10))
	rec = httptest.NewRecorder()
	fx.h.TradeDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trade sheet = %d", rec.Code)
	}
	if out := rec.Body.String(); !strings.Contains(out, "AI suggested stop") || !strings.Contains(out, "taken from an AI suggestion while open") {
		t.Errorf("trade sheet lacks the frozen suggestion or the plan origin")
	}
}

// A refused answer (levels on the wrong side) is reported, not stored, and
// Dismiss drops a stored one.
func TestPositionSuggestion_RefusedAndDismiss(t *testing.T) {
	fx, _ := aiPositionFixture(t, `{"stop": 7290, "target": 7271, "summary": "wrong side"}`)
	fx.post(t, fx.h.PositionSuggestStart, "/positions/x/y/suggest")
	waitTasks(t, fx.h)
	body := fx.sheet(t)
	if !strings.Contains(body, "Suggestion failed") || !strings.Contains(body, "wrong side of the entry") || strings.Contains(body, "Use as plan") {
		t.Errorf("refused answer not reported: %s", body)
	}
	if _, err := fx.q.GetAIOutput(context.Background(), models.GetAIOutputParams{UserID: fx.uid, Kind: aiKindPositionLevels, Ref: aiLevelsRef(fx.acct.ID, "ESM6", 0)}); err == nil {
		t.Errorf("a refused answer must not be stored")
	}

	fx2, _ := aiPositionFixture(t, `{"stop": 7241, "target": 7271, "summary": "fine"}`)
	fx2.post(t, fx2.h.PositionSuggestStart, "/positions/x/y/suggest")
	waitTasks(t, fx2.h)
	if !strings.Contains(fx2.sheet(t), "Use as plan") {
		t.Fatalf("suggestion missing")
	}
	rec := fx2.post(t, fx2.h.PositionSuggestDismiss, "/positions/x/y/suggest/dismiss")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Use as plan") {
		t.Errorf("dismiss = %d, suggestion still shown", rec.Code)
	}
}

// minuteSource serves flat one-minute bars for any window (a cache feed).
type minuteSource struct{ price float64 }

func (m minuteSource) FetchBars(_ context.Context, _ string, _ marketdata.Timeframe, from, to time.Time) ([]marketdata.Bar, error) {
	var out []marketdata.Bar
	for ts := from.Truncate(time.Minute); ts.Before(to) && ts.Before(time.Now()); ts = ts.Add(time.Minute) {
		out = append(out, marketdata.Bar{Time: ts.Unix(), Open: m.price, High: m.price + 1, Low: m.price - 1, Close: m.price, Volume: 5})
	}
	if len(out) == 0 {
		return nil, marketdata.ErrNoData
	}
	return out, nil
}

// With cached one-minute bars the levels prompt carries the session: its
// structure and the multi-timeframe window, not the bare bar slice.
func TestPositionSuggestion_SessionContext(t *testing.T) {
	fx, fp := aiPositionFixture(t, `{"stop": 7241, "target": 7271, "summary": "with structure"}`)
	fx.h.MarketData = &marketdata.CachingSource{DB: fx.h.DB, Source: minuteSource{price: 7252}, Loc: time.UTC}
	w := &marketdata.Warmer{Cache: fx.h.MarketData, Chicago: time.UTC, Every: time.Minute, Roots: func(context.Context) ([]string, error) { return []string{"ES"}, nil }}
	w.Tick(context.Background())
	fx.post(t, fx.h.PositionSuggestStart, "/positions/x/y/suggest")
	waitTasks(t, fx.h)
	fp.mu.Lock()
	user := fp.user
	fp.mu.Unlock()
	for _, want := range []string{`"session"`, `"structure"`, `"levelsNearest"`, `"windows"`, `"15m"`, `"5m"`, `"1m"`, `"barsStoredAt": "1m"`} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt lacks %s", want)
		}
	}
	if strings.Contains(user, `"bars":`) {
		t.Errorf("the bare bar slice should give way to the session")
	}
	if body := fx.sheet(t); !strings.Contains(body, "Use as plan") || strings.Contains(body, "no bars were available") {
		t.Errorf("suggestion with bars should carry no bars warning")
	}
}

// With automatic plans on, an unplanned open position gets the suggestion
// written into its plan without a click: once per cycle (a plan the
// trader clears is not set again), never for a user who did not ask.
func TestAutoPlan(t *testing.T) {
	fx, fp := aiPositionFixture(t, `{"stop": 7241, "target": 7271, "trail": "none", "reasons": {"stop": "swing", "target": "high"}, "summary": "Auto levels."}`)
	ctx := context.Background()
	if n := fx.h.autoPlanUser(ctx, fx.uid); n != 0 {
		t.Fatalf("planned for a user without the setting: %d", n)
	}
	if err := fx.q.SetUserAutoPlan(ctx, models.SetUserAutoPlanParams{AutoPlan: autoPlanSet, ID: fx.uid}); err != nil {
		t.Fatal(err)
	}
	fx.h.autoPlanTick(ctx)
	waitTasks(t, fx.h)
	plans, _ := fx.q.ListOpenPositionPlansForUser(ctx, fx.uid)
	if len(plans) != 1 || plans[0].Source != "ai" || plans[0].Stop == nil || *plans[0].Stop != 7241 || plans[0].Target == nil || *plans[0].Target != 7271 || !strings.Contains(plans[0].Thesis, "Auto levels") {
		t.Fatalf("auto plan = %+v", plans)
	}
	fp.mu.Lock()
	calls := fp.calls
	fp.mu.Unlock()
	if calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
	if body := fx.sheet(t); !strings.Contains(body, "✨ ai") || !strings.Contains(body, "as their plan automatically") {
		t.Errorf("sheet should show the ai plan and the automatic note")
	}

	// The same data version is not read twice; a new version with the plan
	// in place starts nothing; a cleared plan is left cleared.
	fx.h.autoPlanTick(ctx)
	fx.h.LiveData.Bump(fx.uid)
	fx.h.autoPlanTick(ctx)
	if err := fx.q.DeletePositionPlan(ctx, plans[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := fx.h.autoPlanUser(ctx, fx.uid); n != 0 {
		t.Errorf("a cleared plan was set again")
	}
	waitTasks(t, fx.h)
	fp.mu.Lock()
	calls = fp.calls
	fp.mu.Unlock()
	if calls != 1 {
		t.Errorf("model calls = %d after the plan was cleared, want still 1", calls)
	}
	if plans, _ := fx.q.ListOpenPositionPlansForUser(ctx, fx.uid); len(plans) != 0 {
		t.Errorf("plan came back: %+v", plans)
	}
}

// A copied decision is planned once, from its earliest leg, and every leg
// gets the plan.
func TestAutoPlan_Set(t *testing.T) {
	fx, fp := aiPositionFixture(t, `{"stop": 7241, "target": 7271, "trail": "none", "summary": "Set levels."}`)
	ctx := context.Background()
	firm := "lucid"
	follower, err := fx.q.CreateAccount(ctx, models.CreateAccountParams{UserID: fx.uid, Name: "pos-b", Firm: &firm,
		ImportSource: "motivewave_csv", AccountRef: "LFE-POS-B", Currency: "USD", CommissionSource: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	oid := "O-B1"
	if _, err := fx.q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: follower.ID, BrokerExecID: "B1", OrderID: &oid, ImportMethod: "motivewave_csv",
		Symbol: "MESM6", SymbolRoot: "MES", Side: "buy", Qty: 2, Price: 7250.5, ExecTime: fx.t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := fx.q.SetUserAutoPlan(ctx, models.SetUserAutoPlanParams{AutoPlan: autoPlanSet, ID: fx.uid}); err != nil {
		t.Fatal(err)
	}
	if n := fx.h.autoPlanUser(ctx, fx.uid); n != 1 {
		t.Fatalf("started %d, want 1 for the set", n)
	}
	waitTasks(t, fx.h)
	plans, _ := fx.q.ListOpenPositionPlansForUser(ctx, fx.uid)
	if len(plans) != 2 {
		t.Fatalf("plans = %+v, want both legs", plans)
	}
	for _, p := range plans {
		if p.Source != "ai" || p.SetPlan != 1 || p.Stop == nil || *p.Stop != 7241 || p.Target == nil || *p.Target != 7271 {
			t.Errorf("leg plan = %+v", p)
		}
		if p.AccountID == follower.ID && (p.Risk == nil || *p.Risk != 95) {
			t.Errorf("follower risk = %v, want 95", p.Risk)
		}
	}
	fp.mu.Lock()
	calls := fp.calls
	fp.mu.Unlock()
	if calls != 1 {
		t.Errorf("model calls = %d, want 1 for the decision", calls)
	}
}

// A refused answer writes no plan, and the cycle waits before it is tried
// again.
func TestAutoPlan_Refused(t *testing.T) {
	fx, fp := aiPositionFixture(t, `{"stop": 7290, "target": 7271, "summary": "wrong side"}`)
	ctx := context.Background()
	if err := fx.q.SetUserAutoPlan(ctx, models.SetUserAutoPlanParams{AutoPlan: autoPlanSet, ID: fx.uid}); err != nil {
		t.Fatal(err)
	}
	if n := fx.h.autoPlanUser(ctx, fx.uid); n != 1 {
		t.Fatalf("started %d", n)
	}
	waitTasks(t, fx.h)
	if plans, _ := fx.q.ListOpenPositionPlansForUser(ctx, fx.uid); len(plans) != 0 {
		t.Fatalf("a refused answer wrote a plan: %+v", plans)
	}
	if n := fx.h.autoPlanUser(ctx, fx.uid); n != 0 {
		t.Errorf("retried at once")
	}
	fp.mu.Lock()
	calls := fp.calls
	fp.mu.Unlock()
	if calls != 1 {
		t.Errorf("model calls = %d", calls)
	}
	if body := fx.sheet(t); !strings.Contains(body, "Suggestion failed") {
		t.Errorf("the sheet should report the failed attempt once")
	}
}

// In the suggest mode the suggestion is made and waits on the sheet;
// nothing is written to the plan.
func TestAutoPlan_SuggestOnly(t *testing.T) {
	fx, fp := aiPositionFixture(t, `{"stop": 7241, "target": 7271, "trail": "none", "summary": "Only suggested."}`)
	ctx := context.Background()
	if err := fx.q.SetUserAutoPlan(ctx, models.SetUserAutoPlanParams{AutoPlan: autoPlanSuggest, ID: fx.uid}); err != nil {
		t.Fatal(err)
	}
	before := fx.h.LiveData.Version(fx.uid)
	if n := fx.h.autoPlanUser(ctx, fx.uid); n != 1 {
		t.Fatalf("started %d", n)
	}
	waitTasks(t, fx.h)
	if plans, _ := fx.q.ListOpenPositionPlansForUser(ctx, fx.uid); len(plans) != 0 {
		t.Fatalf("suggest mode wrote a plan: %+v", plans)
	}
	if fx.h.LiveData.Version(fx.uid) == before {
		t.Errorf("an open sheet would not learn of the suggestion")
	}
	body := fx.sheet(t)
	for _, want := range []string{"stop 7241", "Only suggested", "Use as plan", "you decide whether to use it"} {
		if !strings.Contains(body, want) {
			t.Errorf("sheet lacks %q", want)
		}
	}
	if n := fx.h.autoPlanUser(ctx, fx.uid); n != 0 {
		t.Errorf("suggested twice")
	}
	fp.mu.Lock()
	calls := fp.calls
	fp.mu.Unlock()
	if calls != 1 {
		t.Errorf("model calls = %d", calls)
	}
}

// The setting is a three-way choice in Settings > AI.
func TestSettingsAIAutoPlanSave(t *testing.T) {
	fx := newPositionFixture(t)
	fx.h.AITasks = ai.NewTasks(nil, 1)
	for _, c := range []struct {
		name string
		want int64
	}{{"plan", autoPlanSet}, {"suggest", autoPlanSuggest}, {"off", autoPlanOff}} {
		form := url.Values{"auto_plan": {c.name}}
		req := httptest.NewRequest(http.MethodPost, "/settings/ai/autoplan", strings.NewReader(form.Encode())).WithContext(fx.ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		fx.h.SettingsAIAutoPlanSave(rec, req)
		if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "flash=auto-plan") {
			t.Fatalf("save %q = %d %s", c.name, rec.Code, rec.Header().Get("Location"))
		}
		u, _ := fx.q.GetUserByID(context.Background(), fx.uid)
		if u.AutoPlan != c.want {
			t.Errorf("auto_plan after %q = %d, want %d", c.name, u.AutoPlan, c.want)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/ai/autoplan", strings.NewReader("auto_plan=x")).WithContext(fx.ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	fx.h.SettingsAIAutoPlanSave(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown mode = %d", rec.Code)
	}
}
