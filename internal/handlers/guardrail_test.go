package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/guardrail"
	"github.com/w4jnl/trading-journal/internal/models"
)

// The Settings toggle turns the page poll on; a take returns the new
// nudges once (never a stale one) and the day page lists the day's.
func TestGuardrailWeb(t *testing.T) {
	h, ctx, uid := setsFixture(t)
	h.Guardrail = &guardrail.Service{}

	page := func(fn http.HandlerFunc, target string) string {
		req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
		if strings.HasPrefix(target, "/journal/2") {
			req.SetPathValue("date", strings.TrimPrefix(target, "/journal/"))
		}
		rec := httptest.NewRecorder()
		fn(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", target, rec.Code)
		}
		return rec.Body.String()
	}
	if body := page(h.LessonsPage, "/journal/lessons"); strings.Contains(body, `name="tj-guardrail"`) || !strings.Contains(body, "/static/js/guardrail.js") {
		t.Errorf("guardrail off: no poll marker, but the script (Settings button) loads")
	}
	if body := page(h.SettingsPage, "/settings"); !strings.Contains(body, `id="guardrail"`) || !strings.Contains(body, "data-notify-permission") {
		t.Errorf("settings lacks the guardrail card")
	}

	req := httptest.NewRequest(http.MethodPost, "/settings/guardrail", strings.NewReader(url.Values{"on": {"1"}}.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.SettingsGuardrailSave(rec, req)
	if rec.Code != http.StatusSeeOther || !h.guardrailOn(ctx) {
		t.Fatalf("toggle = %d, on %v", rec.Code, h.guardrailOn(ctx))
	}
	if body := page(h.LessonsPage, "/journal/lessons"); !strings.Contains(body, `<meta name="tj-guardrail" content="on">`) {
		t.Errorf("guardrail on: the page should carry the poll marker")
	}

	q := h.Queries
	for _, k := range []string{"old", "revenge-entry"} {
		if _, err := q.InsertNudge(ctx, models.InsertNudgeParams{UserID: uid, Day: "2026-10-09", Key: k, Severity: "warn",
			Title: "Re-entry right after a loss", Detail: "ES long opened 30s after a -80.00 loss.", Lesson: strPtr("Walk away for five minutes."), Href: "/journal/2026-10-09"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DB.Exec(`UPDATE nudges SET created_at = ? WHERE key = 'old'`, time.Now().UTC().Add(-3*time.Hour).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	take := func() []apiNudge {
		req := httptest.NewRequest(http.MethodPost, "/nudges/take", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.NudgesTake(rec, req)
		var out struct{ Nudges []apiNudge }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("take = %d %s", rec.Code, rec.Body.String())
		}
		return out.Nudges
	}
	got := take()
	if len(got) != 1 || got[0].Title != "Re-entry right after a loss" || got[0].Lesson != "Walk away for five minutes." || got[0].Href != "/journal/2026-10-09" {
		t.Fatalf("take: %+v", got)
	}
	if again := take(); len(again) != 0 {
		t.Errorf("a nudge was taken twice: %+v", again)
	}
	var undelivered int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM nudges WHERE delivered_at IS NULL`).Scan(&undelivered)
	if undelivered != 0 {
		t.Errorf("the stale nudge should be marked too: %d left", undelivered)
	}

	if body := page(h.JournalPage, "/journal/2026-10-09"); !strings.Contains(body, "Guardrail nudges") || !strings.Contains(body, "Your lesson: Walk away for five minutes.") {
		t.Errorf("the day page should list the day's nudges")
	}
}

func strPtr(s string) *string { return &s }
