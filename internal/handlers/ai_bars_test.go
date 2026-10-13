package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/secretbox"
	"github.com/w4jnl/trading-journal/web"
)

func minuteBars(n int, start time.Time) []marketdata.Bar {
	out := make([]marketdata.Bar, n)
	for i := range out {
		p := 7000 + float64(i)
		out[i] = marketdata.Bar{Time: start.Add(time.Duration(i) * time.Minute).Unix(), Open: p, High: p + 1, Low: p - 1, Close: p + 0.5}
	}
	return out
}

// The window runs from 30 bars before the entry to 15 after the exit; a
// longer one is merged into wider bars; no markers keeps the last 90.
func TestAIBarWindow(t *testing.T) {
	start := time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
	bars := minuteBars(200, start)
	at := func(i int) int64 { return bars[i].Time + 20 }
	p := chartPanel{BarTimeframe: "1m", Bars: bars, Markers: []chartMarker{{T: at(100), Role: "entry"}, {T: at(110), Role: "exit"}}}
	w := aiBarWindow(p, time.UTC)
	rows := w["rows"].([][]any)
	if w["tf"] != "1m" || len(rows) != 56 || rows[0][0] != "14:10" || w["entry"] != "14:40" || w["exit"] != "14:50" {
		t.Errorf("window tf=%v rows=%d first=%v entry=%v exit=%v", w["tf"], len(rows), rows[0][0], w["entry"], w["exit"])
	}
	if rows[0][1] != 7070.0 || rows[0][2] != 7071.0 {
		t.Errorf("row = %v", rows[0])
	}
	// A long hold: 10..190 plus margins = 200 bars -> merged by 3.
	p.Markers = []chartMarker{{T: at(10), Role: "entry"}, {T: at(190), Role: "exit"}}
	w = aiBarWindow(p, time.UTC)
	rows = w["rows"].([][]any)
	if w["tf"] != "3m" || len(rows) != 67 || rows[0][2] != 7003.0 {
		t.Errorf("merged tf=%v rows=%d first=%v", w["tf"], len(rows), rows[0])
	}
	p.Markers = nil
	if rows := aiBarWindow(p, time.UTC)["rows"].([][]any); len(rows) != 90 || rows[0][0] != "14:50" {
		t.Errorf("no markers: %d rows from %v", len(rows), rows[0][0])
	}
	if aiBarWindow(chartPanel{}, time.UTC) != nil {
		t.Error("window without bars")
	}
}

// imageClient accepts or refuses images, recording what it was sent.
type imageClient struct {
	refuse      int // status for image calls, 0 = accept
	imageCalls  int
	textCalls   int
	lastUser    string
	lastImageOK bool
}

func (c *imageClient) Generate(_ context.Context, _, user string) (string, error) {
	c.textCalls++
	c.lastUser = user
	return "text review", nil
}

func (c *imageClient) GenerateImages(_ context.Context, _, user string, pngs [][]byte) (string, error) {
	c.imageCalls++
	c.lastUser = user
	if c.refuse != 0 {
		return "", &ai.StatusError{Code: c.refuse, Msg: "no images here"}
	}
	c.lastImageOK = len(pngs) == 1
	return "image review", nil
}

// A model that refuses the image gets the review without it; a key or rate
// problem is not retried.
func TestAIGenerateImageFallback(t *testing.T) {
	ctx := context.Background()
	user := "The attached image is this trade's chart (candles with the entry and exit marked).\n\n{}"
	ok := &imageClient{}
	if out, err := aiGenerate(ctx, ok, "s", user, []byte("png")); err != nil || out != "image review" || !ok.lastImageOK {
		t.Errorf("vision model: %q %v", out, err)
	}
	refuse := &imageClient{refuse: 400}
	if out, err := aiGenerate(ctx, refuse, "s", user, []byte("png")); err != nil || out != "text review" || refuse.textCalls != 1 || refuse.lastUser != "{}" {
		t.Errorf("text-only model: %q %v calls=%d user=%q", out, err, refuse.textCalls, refuse.lastUser)
	}
	for _, code := range []int{401, 429, 503} {
		c := &imageClient{refuse: code}
		var se *ai.StatusError
		if _, err := aiGenerate(ctx, c, "s", user, []byte("png")); !errors.As(err, &se) || c.textCalls != 0 {
			t.Errorf("status %d retried without the image (err %v)", code, err)
		}
	}
	plain := &imageClient{}
	if _, err := aiGenerate(ctx, plain, "s", "{}", nil); err != nil || plain.imageCalls != 0 || plain.textCalls != 1 {
		t.Error("no image: should be a text call")
	}
}

// A trade review carries the bar window, and the chart image when the user
// sends it.
func TestTradeReviewSendsBarsAndChart(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	sym, _ := marketdata.YahooSymbol("ES")
	entry := time.Date(2026, 10, 9, 14, 1, 30, 0, time.UTC)
	exit := entry.Add(3 * time.Minute)
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + itoa64(uid) + `, 'mine', 'motivewave_csv', 'A-1')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
		(10, 1, 'ESZ6', 'ES', 'long', 1, 7031, 7034, ?, ?, 'closed', 150)`, entry, exit); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO journal_entries (id, user_id, trade_id, entry_date, body, scope) VALUES (1, ` + itoa64(uid) + `, 10, '2026-10-09', 'note', 'trade')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO executions (id, account_id, broker_exec_id, symbol, symbol_root, side, qty, price, exec_time) VALUES
		(1, 1, 'e1', 'ESZ6', 'ES', 'buy', 1, 7031, ?), (2, 1, 'e2', 'ESZ6', 'ES', 'sell', 1, 7034, ?)`, entry, exit); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO trade_executions (trade_id, execution_id, role, qty_used) VALUES (10, 1, 'entry', 1), (10, 2, 'exit', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, b := range minuteBars(60, time.Date(2026, 10, 9, 13, 30, 0, 0, time.UTC)) {
		if _, err := conn.Exec(`INSERT INTO price_bars VALUES (?, '1m', ?, ?, ?, ?, ?, 0, 0)`, sym, b.Time, b.Open, b.High, b.Low, b.Close); err != nil {
			t.Fatal(err)
		}
	}
	q := models.New(conn)
	box, _ := secretbox.New(bytes.Repeat([]byte{0x33}, 32))
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC,
		MarketData: &marketdata.CachingSource{DB: conn, Loc: time.UTC},
		AISecrets:  box, AIJobs: ai.NewJobManager(nil), AIReviews: ai.NewReviews(nil, 2)}
	fp := newFakeProvider(t, "sk-bars-key-0001", "- fine.\n\n<!-- tj-ai:verdict tag=good strategy=none mistakes=none emotions=none -->")
	withPresetURL(t, "openai", fp.URL)
	enc, nonce, _ := box.Seal("sk-bars-key-0001")
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})
	if err := q.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{UserID: uid, Provider: "openai", Model: "gpt-x", ApiKeyEnc: enc, ApiKeyNonce: nonce}); err != nil {
		t.Fatal(err)
	}

	review := func() map[string]any {
		if _, err := h.analyzeEntry(ctx, 1, true); err != nil {
			t.Fatal(err)
		}
		var sent struct {
			Entry struct {
				Chart struct {
					Panels []map[string]any `json:"panels"`
				} `json:"chart"`
			} `json:"entry"`
		}
		ctxJSON := fp.user[strings.Index(fp.user, "{"):]
		if err := json.Unmarshal([]byte(ctxJSON), &sent); err != nil || len(sent.Entry.Chart.Panels) == 0 {
			t.Fatalf("no chart in the context: %v", err)
		}
		return sent.Entry.Chart.Panels[0]["bars"].(map[string]any)
	}
	bars := review()
	if bars["tf"] != "1m" || bars["entry"] != "14:01" || bars["exit"] != "14:04" || len(bars["rows"].([]any)) < 45 || fp.images != 0 {
		t.Errorf("bars = tf %v entry %v exit %v rows %d, images %d", bars["tf"], bars["entry"], bars["exit"], len(bars["rows"].([]any)), fp.images)
	}
	if !strings.Contains(fp.system, "entry.chart.panels[].bars") {
		t.Error("the prompt does not explain the bars")
	}
	// The user turns the chart image on.
	if err := q.SetUserAISendChart(ctx, models.SetUserAISendChartParams{AiSendChart: 1, ID: uid}); err != nil {
		t.Fatal(err)
	}
	review()
	if fp.images != 1 || !strings.HasPrefix(fp.user, "The attached image is this trade's chart") {
		t.Errorf("chart image not sent: images=%d user=%.60q", fp.images, fp.user)
	}
}

func TestSettingsAIChartSave(t *testing.T) {
	h, _, q := newAIHandlers(t)
	for _, c := range []struct {
		form string
		want int64
	}{{"send_chart=1", 1}, {"", 0}} {
		rec := aiPost(h.SettingsAIChartSave, aiUser, "/settings/ai/chart", mustParseQuery(c.form))
		if loc := rec.Header().Get("Location"); rec.Code != 303 || !strings.Contains(loc, "flash=ai-chart") {
			t.Errorf("%q: %d %q", c.form, rec.Code, loc)
		}
		if u, _ := q.GetUserByID(context.Background(), 7); u.AiSendChart != c.want {
			t.Errorf("%q: ai_send_chart = %d", c.form, u.AiSendChart)
		}
	}
}

func mustParseQuery(s string) map[string][]string {
	out := map[string][]string{}
	for _, kv := range strings.Split(s, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = append(out[k], v)
		}
	}
	return out
}
