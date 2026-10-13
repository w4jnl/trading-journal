package handlers

import (
	"bytes"
	"context"
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

// labFixture: ten earlier ES long winners (2 pts of heat, 6 pts best),
// then trade 50 (long 2 ES at 7000, 14:10) with 1m bars around it - a dip
// to 6995 at 13:55 before the entry, a run to 7013.75 after it.
func labFixture(t *testing.T, reply string) (*Handlers, context.Context, *fakeProvider) {
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
	fp := newFakeProvider(t, "sk-lab-key-0001", reply)
	withPresetURL(t, "openai", fp.URL)
	enc, nonce, _ := box.Seal("sk-lab-key-0001")
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	if err := q.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{UserID: uid, Provider: "openai", Model: "gpt-x", ApiKeyEnc: enc, ApiKeyNonce: nonce}); err != nil {
		t.Fatal(err)
	}
	return h, ctx, fp
}

// The lab replays the selection's decisions on their bars: the grid from
// the winners' heat and best, the baseline of the trader's exits, the best
// cell marked, the scatter drawn; Explain stores the model's reading.
func TestLabPage(t *testing.T) {
	h, ctx, fp := labFixture(t, "- **Best rule:** stop 2, target 6.\n- **Confidence:** small sample.")
	// The ten history trades have no bars; give each a run up to +7 after
	// the entry so the replay has something to read.
	sym := "ES=F"
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		en := day.AddDate(0, 0, -i-1).Add(14 * time.Hour)
		for m := 0; m < 20; m++ {
			p := 7000 + float64(m)*0.4
			if _, err := h.DB.Exec(`INSERT INTO price_bars VALUES (?, '1m', ?, ?, ?, ?, ?, 0, 0)`, sym, en.Add(time.Duration(m)*time.Minute).Unix(), p, p+1, p-1, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	get := func(target string) string {
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.LabPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("lab = %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	page := get("/analytics/lab?from=2026-09-01&to=2026-10-10&root=ES")
	for _, want := range []string{"<strong class=\"num\">11</strong> decisions replayed", "Your exits:", "stop ↓ · target →", "lab-pt-win", "$/ct", "Explain this grid"} {
		if !strings.Contains(page, want) {
			t.Errorf("lab lacks %q", want)
		}
	}
	// 11 decisions: no cell has the 20 a "best rule" needs.
	if strings.Contains(page, "lab-best") || strings.Contains(page, "Best rule here") {
		t.Error("a best rule was named on too small a sample")
	}
	// Explicit axes and a direction that matches nothing.
	if p := get("/analytics/lab?from=2026-09-01&to=2026-10-10&root=ES&stops=1,3&targets=2,0"); !strings.Contains(p, ">1 pts</th>") || !strings.Contains(p, ">none</th>") {
		t.Error("explicit axes not used")
	}
	if p := get("/analytics/lab?from=2026-09-01&to=2026-10-10&direction=short"); !strings.Contains(p, "No decisions with cached bars") {
		t.Error("empty selection not explained")
	}

	req := httptest.NewRequest(http.MethodPost, "/analytics/lab/explain?from=2026-09-01&to=2026-10-10&root=ES", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.LabExplain(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/analytics/lab?") {
		t.Fatalf("explain = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	waitTasks(t, h)
	if !strings.Contains(fp.system, "stop/target lab") || !strings.Contains(fp.user, `"baseline"`) || !strings.Contains(fp.user, `"decisions": 11`) {
		t.Errorf("the model got the wrong material: %.200q", fp.user)
	}
	if p := get("/analytics/lab?from=2026-09-01&to=2026-10-10&root=ES"); !strings.Contains(p, "<strong>Best rule:</strong> stop 2, target 6.") || !strings.Contains(p, "Read it again") {
		t.Error("the reading is not shown")
	}
	// Another selection has its own reading.
	if p := get("/analytics/lab?from=2026-09-01&to=2026-10-10&root=ES&timestop=5"); strings.Contains(p, "Best rule:") {
		t.Error("a reading leaked into another selection")
	}
	_ = context.Background()
}

func TestLabFiltersKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/analytics/lab?strategy=ORB&root=es&direction=long&timestop=15&stops=4,2&targets=0,6", nil)
	f := parseLabFilters(r, time.UTC)
	if f.Root != "ES" || f.TimeStop != 15 || len(f.Stops) != 2 || f.Stops[0] != 2 || f.Targets[0] != 0 || f.From == "" {
		t.Errorf("filters = %+v", f)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/analytics/lab?"+f.query().Encode(), nil)
	if parseLabFilters(r2, time.UTC).key() != f.key() {
		t.Error("the key does not survive a round trip")
	}
}
