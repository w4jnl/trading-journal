package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

func TestCleanPlanDraft(t *testing.T) {
	d := cleanPlanDraft("2026-10-14", "3", "9:30", "$450.4", []any{"ES 7860 gamma flip", "", " ES 7900 call wall "}, "Wait for the retest.\n\nStop after two losers.", []string{" a ", ""})
	if d.MaxTrades == nil || *d.MaxTrades != 3 || d.StopTime != "09:30" || d.MaxLoss == nil || *d.MaxLoss != 450 ||
		d.Levels != "ES 7860 gamma flip\nES 7900 call wall" || d.Notes != "Wait for the retest.\nStop after two losers." || len(d.Reasons) != 1 {
		t.Errorf("clean draft: %+v", d)
	}
	d = cleanPlanDraft("2026-10-14", 0.0, "25:00", -5.0, nil, 7.0, nil)
	if d.MaxTrades != nil || d.StopTime != "" || d.MaxLoss != nil || d.Levels != "" || d.Notes != "" {
		t.Errorf("out-of-range fields must be dropped: %+v", d)
	}
	if d := cleanPlanDraft("2026-10-14", 60.0, nil, 2e6, nil, nil, nil); d.MaxTrades != nil || d.MaxLoss != nil {
		t.Errorf("a 60-decision cap or a 2M loss is not a plan: %+v", d)
	}
}

func TestPlanFormOverlay(t *testing.T) {
	three, loss := int64(3), 500.0
	plan := &dayplan.Plan{Bias: "long", MaxTrades: &three, MaxLoss: &loss, Levels: "ES 7850 overnight low\nES 7860 gamma flip", Notes: "My own note."}
	four, dloss := int64(4), 450.0
	f := newPlanForm(plan, &planDraftView{planDraftData: planDraftData{MaxTrades: &four, StopTime: "10:30", MaxLoss: &dloss,
		Levels: "ES 7860 gamma flip\nES 7900 call wall", Notes: "Wait for the retest."}})
	want := planForm{Bias: "long", MaxTrades: "4", StopTime: "10:30", MaxLoss: "450",
		Levels: "ES 7850 overnight low\nES 7860 gamma flip\nES 7900 call wall", Notes: "My own note.\nWait for the retest."}
	if f != want {
		t.Errorf("overlay:\n got %+v\nwant %+v", f, want)
	}
	if f := newPlanForm(plan, nil); f.MaxTrades != "3" || f.MaxLoss != "500" || f.StopTime != "" || f.Bias != "long" {
		t.Errorf("plan only: %+v", f)
	}
	if f := newPlanForm(nil, nil); f != (planForm{}) {
		t.Errorf("no plan: %+v", f)
	}
}

// The plan card drafts today's plan in the background from the evidence
// (here an open lesson), prefills the form under the trader's own plan,
// swaps itself in when the task is done, and saving the plan drops the
// draft. A past day cannot be drafted.
func TestPlanDraftFlow(t *testing.T) {
	h, ctx, uid := setsFixture(t)
	fp := useOwnProvider(t, h, uid, `{"maxTrades": 4, "stopTime": "10:30", "maxLoss": "450", "levels": "ES 7860 gamma flip\nES 7900 call wall",
		"notes": ["Wait for the retest before entering."], "reasons": ["Your usual day is 4 decisions.", "Losing days run to 450 per account."]}`)
	h.AITasks = ai.NewTasks(nil, 1)
	today := time.Now().UTC().Format("2006-01-02")
	key := "chased"
	if err := h.Queries.InsertLesson(ctx, models.InsertLessonParams{UserID: uid, Day: time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"),
		Scope: "trade", MistakeKey: &key, Text: "Wait for the retest."}); err != nil {
		t.Fatal(err)
	}
	three := int64(3)
	if _, err := dayplan.Save(ctx, h.Queries, uid, dayplan.Plan{Day: today, Bias: "short", MaxTrades: &three, Levels: "ES 7850 overnight low"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	post := func(fn http.HandlerFunc, path, date string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("date", date)
		rec := httptest.NewRecorder()
		fn(rec, req)
		return rec
	}

	if rec := post(h.DayPlanDraft, "/journal/plan/2020-01-02/draft", "2020-01-02", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("drafting a past day = %d, want 400", rec.Code)
	}
	rec := post(h.DayPlanDraft, "/journal/plan/"+today+"/draft", today, url.Values{"back": {"/calendar"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/journal/"+today+"?back=%2Fcalendar#day-plan" {
		t.Fatalf("draft = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	waitTasks(t, h)
	fp.mu.Lock()
	system, user := fp.system, fp.user
	fp.mu.Unlock()
	if !strings.Contains(system, "pre-market plan") || !strings.Contains(user, "Wait for the retest.") || !strings.Contains(user, `"currentPlan"`) {
		t.Errorf("draft prompt: system %.60q, user lacks the lesson or the plan:\n%s", system, user)
	}

	card := h.planCardData(ctx, today, "", nil)
	f, _ := card["PlanForm"].(planForm)
	if card["PlanDraft"] == nil || f.Bias != "short" || f.MaxTrades != "4" || f.StopTime != "10:30" || f.MaxLoss != "450" ||
		f.Levels != "ES 7850 overnight low\nES 7860 gamma flip\nES 7900 call wall" || f.Notes != "Wait for the retest before entering." {
		t.Fatalf("plan card: draft %v form %+v", card["PlanDraft"], f)
	}

	// The page shows the draft, its reasons, the prefilled form and the lesson.
	req := httptest.NewRequest(http.MethodGet, "/journal/"+today, nil).WithContext(ctx)
	req.SetPathValue("date", today)
	page := httptest.NewRecorder()
	h.JournalPage(page, req)
	body := page.Body.String()
	for _, want := range []string{"✨ Drafted plan", "Losing days run to 450 per account.", `name="max_trades" min="1" step="1" inputmode="numeric" value="4"`,
		"Open lessons", "Wait for the retest.", `action="/journal/plan/` + today + `/draft/discard"`} {
		if !strings.Contains(body, want) {
			t.Errorf("day page lacks %q", want)
		}
	}

	// The poll swaps the card in place once the task is done.
	st := httptest.NewRequest(http.MethodGet, "/ai/tasks/status?k=plan:"+today+"&then=plan:"+today+"&back=%2Fcalendar", nil).WithContext(ctx)
	srec := httptest.NewRecorder()
	h.AITaskStatus(srec, st)
	if srec.Code != http.StatusOK || srec.Header().Get("HX-Retarget") != "#day-plan" || !strings.Contains(srec.Body.String(), `id="day-plan"`) ||
		!strings.Contains(srec.Body.String(), `name="back" value="/calendar"`) {
		t.Errorf("status swap = %d %q:\n%s", srec.Code, srec.Header().Get("HX-Retarget"), srec.Body.String())
	}

	// Saving the plan drops the draft.
	if rec := post(h.DayPlanSave, "/journal/plan/"+today, today, url.Values{"max_trades": {"4"}, "stop_time": {"10:30"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.Queries.GetAIOutput(ctx, models.GetAIOutputParams{UserID: uid, Kind: aiKindPlanDraft, Ref: today}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the draft should be gone after saving: %v", err)
	}
}

// Without history there is nothing to draft from: the task fails with a
// reason the card shows.
func TestPlanDraftNeedsHistory(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier()}
	useOwnProvider(t, h, uid, `{"maxTrades": 4}`)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	err := h.draftPlan(ctx, time.Now().UTC().Format("2006-01-02"))
	if err == nil || !strings.Contains(err.Error(), "no history") {
		t.Errorf("draft without history: %v", err)
	}
}
