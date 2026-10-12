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

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The coach on the day page (card, label link that opens the editor with
// the chip checked), through the API, and attached to prepare-day.
func TestCoachOnDayPageAPIAndPrepare(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := strconv.FormatInt(uid, 10)
	// A loss at 14:00, a re-entry 20s later on the same market, and a
	// planned trade whose heat went past its risk.
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'mine', 'motivewave_csv', 'A-1')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, gross_pnl, commission, net_pnl, planned_risk, mae_pnl) VALUES
			(10, 1, 'MESZ6', 'MES', 'short', 1, 7863, 7867, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:00:30 +0000 UTC', 'closed', -20, 1, -21, NULL, NULL),
			(11, 1, 'MESZ6', 'MES', 'short', 1, 7866, 7865, '2026-10-09 14:00:50 +0000 UTC', '2026-10-09 14:01:30 +0000 UTC', 'closed', 5, 1, 4, NULL, NULL),
			(12, 1, 'NQZ6', 'NQ', 'long', 1, 20000, 19990, '2026-10-09 15:00:00 +0000 UTC', '2026-10-09 15:05:00 +0000 UTC', 'closed', -20, 1, -21, 50, 80)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})

	// API.
	rec := httptest.NewRecorder()
	h.APICoachChecks(rec, httptest.NewRequest(http.MethodGet, "/api/v1/coach?day=2026-10-09", nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("coach api -> %d: %s", rec.Code, rec.Body.String())
	}
	var rep struct {
		Date      string `json:"date"`
		Trades    int    `json:"trades"`
		Decisions int    `json:"decisions"`
		Findings  []struct {
			Key      string  `json:"key"`
			TradeIDs []int64 `json:"tradeIds"`
			Label    *struct {
				Key string `json:"key"`
			} `json:"label"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Date != "2026-10-09" || rep.Trades != 3 || rep.Decisions != 3 || len(rep.Findings) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Findings[0].Key != "revenge-entry" || rep.Findings[0].TradeIDs[0] != 11 || rep.Findings[0].Label.Key != "revenge" ||
		rep.Findings[1].Key != "past-stop" || rep.Findings[1].TradeIDs[0] != 12 {
		t.Errorf("findings = %+v", rep.Findings)
	}
	rec = httptest.NewRecorder()
	h.APICoachChecks(rec, httptest.NewRequest(http.MethodGet, "/api/v1/coach?day=nonsense", nil).WithContext(ctx))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad day -> %d, want 400", rec.Code)
	}

	// Day page: the card with both findings and the label link.
	page := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		req.SetPathValue("date", "2026-10-09")
		rec := httptest.NewRecorder()
		h.JournalPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	body := page("/journal/2026-10-09")
	for _, want := range []string{
		`Coach · <span class="num">2</span> findings`, `coach-item coach-item--warn`, `Re-entry right after a loss`, `Price went past the planned stop`,
		`href="/journal/2026-10-09?edit=1&mistake=revenge#journal-entry"`, `+ Revenge trade</a>`, `href="/trades?sheet=11&back=%2Fjournal%2F2026-10-09"`,
		`norms need 5 trading days of history`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("day page missing %q", want)
		}
	}
	// The label link: editor shown with the chip checked, nothing saved.
	body = page("/journal/2026-10-09?edit=1&mistake=revenge")
	if !strings.Contains(body, `name="mistake" value="revenge" checked`) || strings.Contains(body, `class="journal-form" hidden`) {
		t.Error("label link should open the editor with the chip checked")
	}
	var labels int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM journal_entry_labels`).Scan(&labels)
	if labels != 0 {
		t.Errorf("pre-checking a chip must not save: %d labels", labels)
	}

	// prepare-day carries the checks.
	prep, err := h.prepareDayEntries(ctx, "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	if prep.Checks == nil || len(prep.Checks.Findings) != 2 || prep.Checks.Date != "2026-10-09" {
		t.Errorf("prepare-day checks = %+v", prep.Checks)
	}
	b, _ := json.Marshal(prep)
	if !strings.Contains(string(b), `"checks":{"date":"2026-10-09"`) || !strings.Contains(string(b), `"entries":[`) {
		t.Errorf("prepare-day json: %s", b)
	}
	// Narrowed to one entry's trades: only the finding on trade 12.
	if n := coachFor(prep.Checks, []int64{12}); len(n.Findings) != 1 || n.Findings[0].Key != "past-stop" {
		t.Errorf("coachFor(12) = %+v", n.Findings)
	}
}
