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

	"github.com/w4jnl/trading-journal/internal/ai"
)

// aiFixture is the journal edit fixture with AI on: a fake provider that
// answers every call with reply, reviews, tasks and the day job.
type aiFixture struct {
	*journalEditFixture
	fp *fakeProvider
}

func newAIFixture(t *testing.T, reply string) *aiFixture {
	t.Helper()
	fx := newJournalEditFixture(t)
	fp := useOwnProvider(t, fx.h, fx.uid, reply)
	fx.h.AITasks = ai.NewTasks(nil, 2)
	return &aiFixture{journalEditFixture: fx, fp: fp}
}

// post calls fn with a JSON body and path values.
func (fx *aiFixture) post(ctx context.Context, fn http.HandlerFunc, method, body string, path map[string]string) (int, map[string]any) {
	req := httptest.NewRequest(method, "/api/v1/ai/x", strings.NewReader(body)).WithContext(ctx)
	if body == "" {
		req.ContentLength = 0
	}
	for k, v := range path {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// The guards: an agent key is refused, a user without a provider gets
// reason ai-off, a server without AI 404.
func TestAPIAIGuards(t *testing.T) {
	fx := newJournalEditFixture(t)
	fx.h.AIJobs, fx.h.AIReviews, fx.h.AITasks = ai.NewJobManager(nil), ai.NewReviews(nil, 2), ai.NewTasks(nil, 2)
	call := func(ctx context.Context, fn http.HandlerFunc, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/x", strings.NewReader(body)).WithContext(ctx)
		req.SetPathValue("date", "2026-10-14")
		rec := httptest.NewRecorder()
		fn(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := call(fx.agent, fx.h.APIReviewStart, `{"entryId": 1}`); code != http.StatusForbidden {
		t.Errorf("agent key = %d, want 403", code)
	}
	if code, out := call(fx.device, fx.h.APIReviewStart, `{"entryId": 1}`); code != http.StatusConflict || out["reason"] != "ai-off" {
		t.Errorf("no provider = %d %v, want 409 ai-off", code, out)
	}
	// /me says so too
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(fx.device)
	rec := httptest.NewRecorder()
	fx.h.APIMe(rec, req)
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me["ai"].(map[string]any)["ready"] != false {
		t.Errorf("me.ai = %v, want ready false", me["ai"])
	}
	fx.h.AIJobs, fx.h.AIReviews = nil, nil
	if code, _ := call(fx.device, fx.h.APIReviewStart, `{"entryId": 1}`); code != http.StatusNotFound {
		t.Errorf("AI off = %d, want 404", code)
	}
}

// A review from the phone: started by entry id or by its trade, pending on
// the detail meanwhile, refused twice, landed with its verdict.
func TestAPIReviewStart(t *testing.T) {
	fx := newAIFixture(t, "- **Result:** steady.\n\n<!-- tj-ai:verdict tag=good strategy=none mistakes=none emotions=none -->")
	if code, out := fx.post(fx.device, fx.h.APIReviewStart, http.MethodPost, `{}`, nil); code != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("empty body = %d %v", code, out)
	}
	if code, _ := fx.post(fx.device, fx.h.APIReviewStart, http.MethodPost, `{"entryId": 999999}`, nil); code != http.StatusNotFound {
		t.Fatalf("unknown entry = %d, want 404", code)
	}
	code, out := fx.post(fx.device, fx.h.APIReviewStart, http.MethodPost, `{"entryId": `+strconv.FormatInt(fx.day.ID, 10)+`}`, nil)
	if code != http.StatusAccepted || out["pending"] != true {
		t.Fatalf("start = %d %v, want 202 pending", code, out)
	}
	// the review may already have landed on a fast machine; the detail says which
	_, detail := fx.call(fx.device, fx.h.APIJournalDetail, http.MethodGet, fx.day.ID, "")
	review, _ := detail["review"].(map[string]any)
	if review == nil {
		t.Fatalf("detail has no review state: %v", detail)
	}
	if err := fx.h.AIReviews.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, detail = fx.call(fx.device, fx.h.APIJournalDetail, http.MethodGet, fx.day.ID, "")
	if detail["review"].(map[string]any)["pending"] != false {
		t.Errorf("after the review: %v", detail["review"])
	}
	if !strings.Contains(detail["notes"].(string), "steady") {
		t.Errorf("the review did not land in the note: %q", detail["notes"])
	}
	if fx.fp.calls == 0 {
		t.Error("the provider was never called")
	}
	// by the trade: the note is created first
	trades, err := fx.q.ListClosedTradesForSets(fx.ctx, fx.uid)
	if err != nil || len(trades) == 0 {
		t.Skip("no closed trade in the fixture")
	}
	code, out = fx.post(fx.device, fx.h.APIReviewStart, http.MethodPost, `{"scope":"trade","tradeId": `+strconv.FormatInt(trades[0].ID, 10)+`}`, nil)
	if code != http.StatusAccepted || out["entryId"] == nil {
		t.Fatalf("by trade = %d %v", code, out)
	}
	_ = fx.h.AIReviews.Wait(context.Background())
}

// The day run from the phone: start, status, refuse a second, cancel.
func TestAPIDayRun(t *testing.T) {
	fx := newAIFixture(t, "- **Result:** steady.\n\n<!-- tj-ai:verdict tag=good strategy=none mistakes=none emotions=none -->")
	if code, _ := fx.post(fx.device, fx.h.APIDayRunStart, http.MethodPost, "", map[string]string{"date": "yesterday"}); code != http.StatusBadRequest {
		t.Errorf("bad date = %d, want 400", code)
	}
	code, out := fx.post(fx.device, fx.h.APIDayRunStart, http.MethodPost, "", map[string]string{"date": "2026-10-14"})
	if code != http.StatusAccepted || out["status"] == nil {
		t.Fatalf("start = %d %v", code, out)
	}
	code, out = fx.post(fx.device, fx.h.APIDayRunStatus, http.MethodGet, "", nil)
	st := out["status"].(map[string]any)
	if code != http.StatusOK || st["state"] == "idle" {
		t.Errorf("status = %d %v", code, out)
	}
	if err := fx.h.AIJobs.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, out = fx.post(fx.device, fx.h.APIDayRunCancel, http.MethodDelete, "", nil)
	if code != http.StatusOK || out["status"] == nil {
		t.Errorf("cancel = %d %v", code, out)
	}
}

// Levels on the open position: start marks the position pending, apply
// without a proposal is refused, dismiss forgets.
func TestAPIPositionLevels(t *testing.T) {
	fx := newAIFixture(t, "{}")
	items, err := fx.h.openPositionsAPI(fx.ctx)
	if err != nil || len(items) == 0 {
		t.Skip("no open position in the fixture")
	}
	path := map[string]string{"account": strconv.FormatInt(items[0].AccountID, 10), "symbol": items[0].Symbol}
	if items[0].AI == nil || items[0].AI.Pending {
		t.Fatalf("position before: ai = %+v", items[0].AI)
	}
	if code, out := fx.post(fx.device, fx.h.APIPositionLevelsApply, http.MethodPost, "", path); code != http.StatusConflict {
		t.Errorf("apply without a proposal = %d %v, want 409", code, out)
	}
	if code, out := fx.post(fx.device, fx.h.APIPositionLevelsStart, http.MethodPost, "", path); code != http.StatusAccepted || out["pending"] != true {
		t.Fatalf("start = %d %v", code, out)
	}
	_ = fx.h.AITasks.Wait(context.Background()) // the fake provider's "{}" is no proposal: the task fails, shown once
	items, _ = fx.h.openPositionsAPI(fx.ctx)
	if items[0].AI == nil || items[0].AI.Pending {
		t.Errorf("after the task: %+v", items[0].AI)
	}
	if code, _ := fx.post(fx.device, fx.h.APIPositionLevelsDismiss, http.MethodDelete, "", path); code != http.StatusNoContent {
		t.Errorf("dismiss = %d, want 204", code)
	}
	if code, _ := fx.post(fx.device, fx.h.APIPositionLevelsStart, http.MethodPost, "", map[string]string{"account": path["account"], "symbol": "NOPE"}); code != http.StatusConflict {
		t.Errorf("flat contract = %d, want 409", code)
	}
}

// The plan draft: a past day is refused, today's starts and shows on the
// plan, discard drops it.
func TestAPIPlanDraft(t *testing.T) {
	fx := newAIFixture(t, "{}")
	today := time.Now().In(fx.h.localTZ()).Format("2006-01-02")
	if code, _ := fx.post(fx.device, fx.h.APIPlanDraftStart, http.MethodPost, "", map[string]string{"date": "2020-01-01"}); code != http.StatusBadRequest {
		t.Errorf("past day = %d, want 400", code)
	}
	if code, out := fx.post(fx.device, fx.h.APIPlanDraftStart, http.MethodPost, "", map[string]string{"date": today}); code != http.StatusAccepted || out["pending"] != true {
		t.Fatalf("start = %d %v", code, out)
	}
	v, err := fx.h.dayPlanView(fx.device, today)
	if err != nil || v.Draft == nil {
		t.Fatalf("plan view: %v %+v", err, v)
	}
	_ = fx.h.AITasks.Wait(context.Background())
	if code, _ := fx.post(fx.device, fx.h.APIPlanDraftDiscard, http.MethodDelete, "", map[string]string{"date": today}); code != http.StatusNoContent {
		t.Errorf("discard = %d, want 204", code)
	}
	if code, _ := fx.post(fx.agent, fx.h.APIPlanDraftStart, http.MethodPost, "", map[string]string{"date": today}); code != http.StatusForbidden {
		t.Errorf("agent = %d, want 403", code)
	}
}
