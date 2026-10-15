package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Today carries the open position, the plan's budgets, the nudges, the
// curve after a close, and the equity once an anchor exists.
func TestAPIToday(t *testing.T) {
	fx := newPositionFixture(t)
	today := time.Now().UTC().Format("2006-01-02")
	max := int64(3)
	loss := 500.0
	if _, err := dayplan.Save(fx.ctx, fx.q, fx.uid, dayplan.Plan{Day: today, Bias: "long", MaxTrades: &max, MaxLoss: &loss}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.q.InsertNudge(fx.ctx, models.InsertNudgeParams{UserID: fx.uid, Day: today, Key: "unplanned-open", Ref: "1", Severity: "warn", Title: "No stop recorded on ESM6", Detail: "d", Href: "/trades?status=open&sheet=pos:" + strconv.FormatInt(fx.acct.ID, 10) + ":ESM6"}); err != nil {
		t.Fatal(err)
	}
	code, out := apiGet(fx.ctx, fx.h.APIToday, "/api/v1/today")
	if code != http.StatusOK {
		t.Fatalf("today = %d %v", code, out)
	}
	if out["day"] != today || len(out["positions"].([]any)) != 1 || out["lastFillAt"] == nil {
		t.Errorf("today = %v", out)
	}
	sess := out["session"].(map[string]any)
	if _, ok := sess["inGlobex"].(bool); !ok || sess["rthStart"] == nil {
		t.Errorf("session = %v", sess)
	}
	plan := out["plan"].(map[string]any)
	if plan["maxTrades"] != 3.0 || plan["maxLoss"] != 500.0 || plan["tradesUsed"] != 0.0 || plan["plan"].(map[string]any)["bias"] != "long" {
		t.Errorf("plan = %v", plan)
	}
	nudges := out["nudges"].(map[string]any)
	if nudges["unseen"] != 1.0 || nudges["badge"] != 1.0 || nudges["items"].([]any)[0].(map[string]any)["route"] != "position/"+strconv.FormatInt(fx.acct.ID, 10)+"/ESM6" {
		t.Errorf("nudges = %v", nudges)
	}
	if out["equity"].(map[string]any)["known"] != false || len(out["curve"].([]any)) != 0 || out["lastTrade"] != nil {
		t.Errorf("fresh day = equity %v curve %v last %v", out["equity"], out["curve"], out["lastTrade"])
	}

	// A close: stats, the curve, the last trade and the loss budget move.
	closeFixturePosition(t, fx, 7241) // 10 points against from 7251: -1500 gross
	if err := fx.q.UpsertBalanceAnchor(fx.ctx, models.UpsertBalanceAnchorParams{AccountID: fx.acct.ID, AnchorDate: fx.t0.AddDate(0, 0, -3), Balance: 50000}); err != nil {
		t.Fatal(err)
	}
	_, out = apiGet(fx.ctx, fx.h.APIToday, "/api/v1/today")
	if n := out["stats"].(map[string]any)["trades"]; n != 1.0 {
		t.Errorf("stats after the close = %v", out["stats"])
	}
	if len(out["curve"].([]any)) != 1 || out["lastTrade"].(map[string]any)["direction"] != "long" || len(out["positions"].([]any)) != 0 {
		t.Errorf("after the close: curve %v last %v positions %v", out["curve"], out["lastTrade"], out["positions"])
	}
	plan = out["plan"].(map[string]any)
	if plan["tradesUsed"] != 1.0 || plan["lossUsed"].(float64) < 1500 || plan["worstAccount"] != "pos-a" {
		t.Errorf("budgets = %v", plan)
	}
	eq := out["equity"].(map[string]any)
	if eq["known"] != true || eq["now"].(float64) >= 50000 || eq["underwater"] != true || eq["dayChange"].(float64) >= 0 {
		t.Errorf("equity = %v", eq)
	}
}

// Nudges list newest first with the app route; seen flips once per nudge
// and bumps the version; the web take counts as seen too.
func TestAPINudges(t *testing.T) {
	fx := newPositionFixture(t)
	today := time.Now().UTC().Format("2006-01-02")
	ids := []int64{}
	for i, key := range []string{"stop-time", "cap-reached"} {
		if _, err := fx.q.InsertNudge(fx.ctx, models.InsertNudgeParams{UserID: fx.uid, Day: today, Key: key, Ref: strconv.Itoa(i), Severity: "warn", Title: key, Detail: "d", Href: "/journal/" + today}); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := fx.q.ListNudgesForDay(fx.ctx, models.ListNudgesForDayParams{UserID: fx.uid, Day: today})
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	code, out := apiGet(fx.ctx, fx.h.APINudgesList, "/api/v1/nudges?day=today")
	items := out["items"].([]any)
	if code != http.StatusOK || out["unseen"] != 2.0 || items[0].(map[string]any)["key"] != "cap-reached" || items[0].(map[string]any)["route"] != "day/"+today {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, _ := apiGet(fx.ctx, fx.h.APINudgesList, "/api/v1/nudges?day=soon"); code != http.StatusBadRequest {
		t.Errorf("bad day = %d", code)
	}
	before := fx.h.LiveData.Version(fx.uid)
	seen := func(id int64) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/nudges/x/seen", nil).WithContext(fx.ctx)
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		rec := httptest.NewRecorder()
		fx.h.APINudgeSeen(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := seen(ids[0]); out["seen"] != true {
		t.Errorf("first seen = %v", out)
	}
	if out := seen(ids[0]); out["seen"] != false {
		t.Errorf("second seen = %v", out)
	}
	if out := seen(999999); out["seen"] != false {
		t.Errorf("unknown seen = %v", out)
	}
	if fx.h.LiveData.Version(fx.uid) != before+1 {
		t.Errorf("version moved %d times, want once", fx.h.LiveData.Version(fx.uid)-before)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nudges/seen", strings.NewReader(`{"ids": [`+strconv.FormatInt(ids[1], 10)+`, 999999]}`)).WithContext(fx.ctx)
	rec := httptest.NewRecorder()
	fx.h.APINudgesSeen(rec, req)
	if !strings.Contains(rec.Body.String(), `"seen":1`) {
		t.Errorf("bulk seen = %s", rec.Body.String())
	}
	_, out = apiGet(fx.ctx, fx.h.APINudgesList, "/api/v1/nudges")
	if out["unseen"] != 0.0 {
		t.Errorf("unseen after both = %v", out["unseen"])
	}
	// A web take marks seen as well.
	if _, err := fx.q.InsertNudge(fx.ctx, models.InsertNudgeParams{UserID: fx.uid, Day: today, Key: "max-loss-hit", Ref: "x", Severity: "warn", Title: "t", Detail: "d", Href: "/"}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	fx.h.NudgesTake(rec, httptest.NewRequest(http.MethodPost, "/nudges/take", nil).WithContext(fx.ctx))
	if n, _ := fx.q.CountUnseenNudges(fx.ctx, models.CountUnseenNudgesParams{UserID: fx.uid, Day: today, CreatedAt: time.Now().UTC().Add(-time.Hour)}); n != 0 {
		t.Errorf("a web take left %d unseen", n)
	}
}

// The stream opens with the version, says it again after a bump, keeps
// alive with a comment, and ends with the request.
func TestAPIEvents(t *testing.T) {
	fx := newPositionFixture(t)
	old := sseKeepAlive
	sseKeepAlive = 50 * time.Millisecond
	defer func() { sseKeepAlive = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.h.APIEvents(w, r.WithContext(fx.ctx))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %s", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	lines := make(chan string, 64)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	expect := func(want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %q", want)
				}
				if strings.Contains(l, want) {
					return
				}
			case <-deadline:
				t.Fatalf("no %q within 3s", want)
			}
		}
	}
	expect("retry: 3000")
	v := fx.h.LiveData.Version(fx.uid)
	expect(`data: {"v":` + strconv.FormatUint(v, 10) + `}`)
	expect(": ping")
	fx.h.LiveData.Bump(fx.uid)
	expect(`data: {"v":` + strconv.FormatUint(v+1, 10) + `}`)
	cancel()
	select {
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not end with the request")
	case _, ok := <-lines:
		for ok {
			_, ok = <-lines
		}
	}
}
