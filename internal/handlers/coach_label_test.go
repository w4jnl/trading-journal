package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/auth"
)

// The coach card's "+ <label>" button logs the finding on the day note as the
// trader's own: it creates the note when the day has none, a second click is
// harmless, an unknown label is refused, and a note under review answers 409.
func TestCoachLabelLogsOnTheDayNote(t *testing.T) {
	h, db, _ := newAIHandlers(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /journal/coach/{date}", h.JournalCoachLabel)
	ctx := auth.WithUser(context.Background(), aiUser)
	post := func(day, kind, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/journal/coach/"+day, strings.NewReader(url.Values{"kind": {kind}, "key": {key}}.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := post("2026-10-09", "mistake", "overtraded")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/journal/2026-10-09#coach" {
		t.Fatalf("first click = %d %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	var eid int64
	var scope string
	if err := db.QueryRow(`SELECT id, scope FROM journal_entries WHERE user_id = 7 AND entry_date LIKE '2026-10-09%'`).Scan(&eid, &scope); err != nil || scope != "day" {
		t.Fatalf("day note not created: %v (%s)", err, scope)
	}
	if src := h.labelSources(ctx, eid); src["mistake:overtraded"] != "trader" {
		t.Errorf("label sources after the click = %v", src)
	}
	// Again: the row stays, nothing breaks.
	if rec := post("2026-10-09", "mistake", "overtraded"); rec.Code != http.StatusSeeOther {
		t.Errorf("second click = %d: %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal_entry_labels WHERE journal_entry_id = ?`, eid).Scan(&n); err != nil || n != 1 {
		t.Errorf("labels on the note = %d (%v)", n, err)
	}
	if rec := post("2026-10-09", "mistake", "made-up"); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown label = %d", rec.Code)
	}
	if rec := post("2026-13-09", "mistake", "overtraded"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad date = %d", rec.Code)
	}
	// A note a review is writing refuses the label.
	release, ok := h.AIReviews.Track(7, eid, nil)
	if !ok {
		t.Fatal("could not hold the note")
	}
	if rec := post("2026-10-09", "mistake", "against-plan"); rec.Code != http.StatusConflict {
		t.Errorf("under review = %d: %s", rec.Code, rec.Body.String())
	}
	release(nil)
	if rec := post("2026-10-09", "mistake", "against-plan"); rec.Code != http.StatusSeeOther {
		t.Errorf("after the review = %d: %s", rec.Code, rec.Body.String())
	}
}
