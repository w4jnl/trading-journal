package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/levelmap"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// dailySource answers the vendor's daily series and nothing finer.
type dailySource struct{ days []marketdata.Bar }

func (s *dailySource) FetchBars(_ context.Context, _ string, tf marketdata.Timeframe, from, to time.Time) ([]marketdata.Bar, error) {
	if tf != marketdata.TF1d {
		return nil, marketdata.ErrNoData
	}
	var out []marketdata.Bar
	for _, b := range s.days {
		t := time.Unix(b.Time, 0)
		if !t.Before(from) && !t.After(to) {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, marketdata.ErrNoData
	}
	return out, nil
}

// levelsFixture: a user who traded MES this week, forty vendor days of ES
// history and three days of one-minute bars ending a minute ago.
func levelsFixture(t *testing.T) (*Handlers, context.Context, int64, string) {
	t.Helper()
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	q := models.New(conn)
	now := time.Now().UTC().Truncate(time.Minute)
	acct, err := q.CreateAccount(context.Background(), models.CreateAccountParams{UserID: uid, Name: "alpha", ImportSource: "motivewave_csv", AccountRef: "A", Currency: "USD", CommissionSource: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertExecution(context.Background(), models.InsertExecutionParams{AccountID: acct.ID, BrokerExecID: "X1", ImportMethod: "motivewave_csv",
		Symbol: "MESZ6", SymbolRoot: "MES", Side: "buy", Qty: 1, Price: 6800, ExecTime: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var days []marketdata.Bar
	for i := 45; i >= 1; i-- {
		d := now.AddDate(0, 0, -i)
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		base := 6700 + float64(45-i)*3
		days = append(days, marketdata.Bar{Time: day.Unix(), Open: base + 5, High: base + 40, Low: base, Close: base + 30, Volume: 1000})
	}
	for m := 3 * 24 * 60; m >= 1; m-- {
		at := now.Add(-time.Duration(m) * time.Minute)
		if at.Weekday() == time.Saturday || at.Weekday() == time.Sunday {
			continue
		}
		p := 6830 + 10*float64((m/90)%4)
		if _, err := conn.Exec(`INSERT INTO price_bars VALUES ('ES=F', '1m', ?, ?, ?, ?, ?, 50, 0)`, at.Unix(), p, p+1.5, p-1.5, p+0.5); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier(),
		MarketData: &marketdata.CachingSource{DB: conn, Source: &dailySource{days: days}, Loc: time.UTC, IntradayTTL: time.Hour}}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	return h, ctx, uid, levelmap.TradingDay(now, h.chicago())
}

func levelsMux(h *Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /levels", h.LevelsPage)
	mux.HandleFunc("GET /levels/data/{market}", h.LevelsData)
	mux.HandleFunc("POST /levels/{market}/plan", h.LevelsToPlan)
	mux.HandleFunc("GET /levels/{market}", h.LevelsPage)
	mux.HandleFunc("POST /settings/levels", h.SettingsLevelsSave)
	mux.HandleFunc("GET /api/v1/levels/{market}", h.APILevelsGet)
	mux.HandleFunc("POST /api/v1/levels/{market}", h.APILevelsPost)
	return mux
}

func postJSON(ctx context.Context, mux *http.ServeMux, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// The page and the API build the map from the bars; a chart's post
// overrides the session levels it knows, the newest post wins, bad posts
// are refused, and the zones land in the day plan.
func TestLevelsMapFeedAndPlan(t *testing.T) {
	h, ctx, uid, day := levelsFixture(t)
	mux := levelsMux(h)
	page := serve(ctx, mux, http.MethodGet, "/levels")
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "Levels · ES") || !strings.Contains(body, "prior day high") || !strings.Contains(body, "Put the zones in the day plan") {
		t.Fatalf("page = %d %q", page.Code, between(body, "<h1", "</h1>"))
	}
	if rec := serve(ctx, mux, http.MethodGet, "/levels/SPX"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown market = %d", rec.Code)
	}
	var m levelmap.Map
	rec := serve(ctx, mux, http.MethodGet, "/api/v1/levels/ES")
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("api map = %d %v", rec.Code, err)
	}
	if !m.Sources.Bars || m.Sources.Chart || m.Day != day || m.ATR <= 0 || len(m.Levels) < 10 || len(m.Zones) == 0 || m.Last <= 0 {
		t.Fatalf("map = sources %+v day %s atr %v levels %d zones %d last %v", m.Sources, m.Day, m.ATR, len(m.Levels), len(m.Zones), m.Last)
	}
	find := func(kind string) *levelmap.Level {
		for i := range m.Levels {
			if m.Levels[i].Kind == kind {
				return &m.Levels[i]
			}
		}
		return nil
	}
	if l := find("pdh"); l == nil || l.Source != levelmap.SourceBars {
		t.Errorf("pdh from bars = %+v", l)
	}
	// A record of the day's map exists for the review loop.
	if _, err := h.Queries.GetLevelMap(ctx, models.GetLevelMapParams{UserID: uid, Market: "ES", TradeDay: day, Phase: "session"}); err != nil {
		t.Errorf("no session map stored: %v", err)
	}

	asOf := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	feed := `{"contract":"ESZ6","asOf":"` + asOf + `","mark":6850.25,"session":{"pdh":6901.5,"poc":6877.75,"profileApprox":true,"or":{"minutes":15,"high":6855,"low":6845,"final":true}},"vwap":{"value":6848,"sd1":3},"atr":{"daily14":52,"method":"smma","anchor":6846,"anchoredAt":"rth","mults":[0.5,1]}}`
	rec = postJSON(ctx, mux, "/api/v1/levels/ES", feed)
	var ack struct {
		OK       bool   `json:"ok"`
		TradeDay string `json:"tradeDay"`
		Newest   bool   `json:"newest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || rec.Code != http.StatusAccepted || !ack.OK || !ack.Newest || ack.TradeDay != day {
		t.Fatalf("post = %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(ctx, mux, http.MethodGet, "/api/v1/levels/ES")
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if !m.Sources.Chart || m.Sources.Contract != "ESZ6" || m.Last != 6850.25 || m.ATR != 52 || m.Anchor != "rth" {
		t.Errorf("chart not applied: %+v last %v atr %v anchor %q", m.Sources, m.Last, m.ATR, m.Anchor)
	}
	if l := find("pdh"); l == nil || l.Price != 6901.5 || l.Source != levelmap.SourceChart {
		t.Errorf("pdh after the feed = %+v", l)
	}
	if l := find("poc"); l == nil || l.Price != 6877.75 || !strings.Contains(l.Name, "approx") {
		t.Errorf("poc = %+v", l)
	}
	if l := find("orh"); l == nil || l.Price != 6855 {
		t.Errorf("orh = %+v", l)
	}
	// An older post is kept but not shown.
	older := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	rec = postJSON(ctx, mux, "/api/v1/levels/ES", `{"contract":"ESZ6","asOf":"`+older+`","mark":6000,"session":{"pdh":5000}}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || rec.Code != http.StatusAccepted || ack.Newest {
		t.Errorf("older post = %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(ctx, mux, http.MethodGet, "/api/v1/levels/ES")
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if l := find("pdh"); l == nil || l.Price != 6901.5 {
		t.Errorf("older post replaced the newest: %+v", l)
	}
	// Two charts posting the same second: the fuller post shows, whichever
	// order they arrive in.
	thin := `{"contract":"ESZ6","asOf":"` + asOf + `","mark":6850.25,"session":{"onh":6000,"onl":5990}}`
	if rec := postJSON(ctx, mux, "/api/v1/levels/ES", thin); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"newest":false`) {
		t.Errorf("thin post at the same second = %d %s, want accepted but not newest", rec.Code, rec.Body.String())
	}
	rec = serve(ctx, mux, http.MethodGet, "/api/v1/levels/ES")
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if l := find("onh"); l == nil || l.Price == 6000 {
		t.Errorf("the thin post replaced the full one: %+v", l)
	}
	if l := find("pdh"); l == nil || l.Price != 6901.5 {
		t.Errorf("pdh after the thin post = %+v", l)
	}
	for name, body := range map[string]string{
		"no mark":   `{"contract":"ESZ6","asOf":"` + asOf + `"}`,
		"bad price": `{"contract":"ESZ6","asOf":"` + asOf + `","mark":1,"session":{"pdl":-3}}`,
		"not json":  `{`,
	} {
		if rec := postJSON(ctx, mux, "/api/v1/levels/ES", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", name, rec.Code)
		}
	}
	if rec := postJSON(ctx, mux, "/api/v1/levels/SPX", feed); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown market post = %d", rec.Code)
	}
	// The page's data endpoint carries the bars for the charts.
	var data struct {
		Daily    []marketdata.Bar `json:"daily"`
		Weekly   []marketdata.Bar `json:"weekly"`
		Intraday []marketdata.Bar `json:"intraday"`
	}
	rec = serve(ctx, mux, http.MethodGet, "/levels/data/ES")
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil || len(data.Daily) == 0 || len(data.Weekly) == 0 || len(data.Intraday) == 0 {
		t.Errorf("data = %d daily %d weekly %d intraday %d (%v)", rec.Code, len(data.Daily), len(data.Weekly), len(data.Intraday), err)
	}
	// The zones go into the day plan, one per line.
	req := httptest.NewRequest(http.MethodPost, "/levels/ES/plan", strings.NewReader("day="+day)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/journal/"+day+"#day-plan" {
		t.Fatalf("plan = %d %s", w.Code, w.Header().Get("Location"))
	}
	p, err := dayplan.Get(ctx, h.Queries, uid, day)
	if err != nil || p == nil || !strings.HasPrefix(p.Levels, "ES ") || strings.Count(p.Levels, "\n") < 3 {
		t.Fatalf("plan levels = %v %+v", err, p)
	}
	// A second run appends under a dated line rather than overwriting.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/levels/ES/plan", strings.NewReader("day="+day)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(w, req)
	p2, _ := dayplan.Get(ctx, h.Queries, uid, day)
	if p2 == nil || !strings.Contains(p2.Levels, "From the map") || !strings.HasPrefix(p2.Levels, p.Levels) {
		t.Errorf("second run = %+v", p2)
	}
}

// The markets on the page are the ones traded lately plus the ones pinned
// on Settings; the pin survives a save with nothing ticked as an empty list.
func TestLevelsPinnedMarkets(t *testing.T) {
	h, ctx, _, _ := levelsFixture(t)
	mux := levelsMux(h)
	if got := h.levelsMarkets(ctx, "2026-10-20"); len(got) != 1 || got[0] != "ES" {
		t.Fatalf("markets traded = %v, want [ES]", got)
	}
	post := func(form string) int {
		req := httptest.NewRequest(http.MethodPost, "/settings/levels", strings.NewReader(form)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	if code := post("market=NQ&market=GC&market=SPX&market=NQ"); code != http.StatusSeeOther {
		t.Fatalf("save = %d", code)
	}
	if got := h.levelsMarkets(ctx, "2026-10-20"); strings.Join(got, ",") != "ES,NQ,GC" {
		t.Errorf("markets after the pin = %v, want ES,NQ,GC in display order (SPX dropped, NQ once)", got)
	}
	body := serve(ctx, mux, http.MethodGet, "/levels/NQ").Body.String()
	if !strings.Contains(body, `href="/levels/NQ?day=`) || !strings.Contains(body, "Levels · NQ") {
		t.Errorf("NQ not on the page after the pin")
	}
	if code := post(""); code != http.StatusSeeOther {
		t.Fatalf("clear = %d", code)
	}
	if got := h.levelsMarkets(ctx, "2026-10-20"); len(got) != 1 {
		t.Errorf("markets after clearing = %v", got)
	}
}
