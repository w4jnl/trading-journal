package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

func TestParseAILesson(t *testing.T) {
	for _, c := range []struct {
		in, key, text string
	}{
		{"x\n<!-- tj-ai:lesson key=chased | Wait for the retest instead of buying the break. -->\n<!-- tj-ai:verdict tag=bad -->", "chased", "Wait for the retest instead of buying the break."},
		{"<!-- tj-ai:lesson key=none | Size down after two losers. -->", "", "Size down after two losers."},
		{"<!-- tj-ai:lesson key=bogus | Keep it. -->", "", "Keep it."},
		{"<!-- TJ-AI:LESSON KEY=Late-Exit|Take the target. -->", "late-exit", "Take the target."},
		{"<!-- tj-ai:lesson Just a sentence. -->", "", "Just a sentence."},
		{"no line here", "", ""},
	} {
		l := parseAILesson(c.in)
		if l.Key != c.key || l.Text != c.text {
			t.Errorf("parseAILesson(%q) = %+v, want key %q text %q", c.in, l, c.key, c.text)
		}
	}
	out := stripAILesson(stripAIVerdict("- **Exit:** early.\n\n<!-- tj-ai:lesson key=early-exit | Hold to the target. -->\n<!-- tj-ai:verdict tag=bad -->"))
	if out != "- **Exit:** early." {
		t.Errorf("strip left %q", out)
	}
	for _, scope := range []string{"trade", "day", "week"} {
		p := aiSystemPrompt(scope, "")
		if i, j := strings.Index(p, "tj-ai:lesson"), strings.Index(p, "tj-ai:verdict"); i < 0 || j < 0 || i > j {
			t.Errorf("%s prompt: the lesson line must be asked for before the verdict (%d, %d)", scope, i, j)
		}
	}
	if p := aiSystemPrompt("trade", "Czech"); !strings.Contains(p, "the lesson sentence inside the lesson line is written in Czech") {
		t.Errorf("a non-English review must write the lesson in its language")
	}
}

// A review's lesson line is kept in the ledger and stripped from the note; a
// re-review replaces the note's lesson; the next review is handed the
// trader's recent lessons; lessons about one mistake are counted; the
// ledger page marks them learned; the week's digest lists them.
func TestReviewLessonsLedger(t *testing.T) {
	h, ctx, uid := setsFixture(t)
	fp := useOwnProvider(t, h, uid, "- **Entry:** chased the break.\n\n<!-- tj-ai:lesson key=chased | Wait for the retest. -->\n<!-- tj-ai:verdict tag=bad -->")
	mux := aiButtonMux(h)
	reply := func(s string) {
		fp.mu.Lock()
		fp.reply = s
		fp.mu.Unlock()
	}

	if rec := serve(ctx, mux, http.MethodPost, "/ai/trade/12"); rec.Code != http.StatusOK {
		t.Fatalf("review = %d %s", rec.Code, rec.Body.String())
	}
	settle(t, h)
	var entryID int64
	var body string
	if err := h.DB.QueryRow(`SELECT id, body FROM journal_entries WHERE trade_id = 12`).Scan(&entryID, &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "tj-ai:lesson") || !strings.Contains(body, "chased the break") {
		t.Errorf("note body: the lesson line must be stripped, the review kept:\n%s", body)
	}
	lessons := func() []models.Lesson {
		rows, err := h.Queries.ListLessonsBetween(ctx, models.ListLessonsBetweenParams{UserID: uid, Day: "2000-01-01", Day_2: "2100-01-01"})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	ls := lessons()
	if len(ls) != 1 || ls[0].Text != "Wait for the retest." || ls[0].MistakeKey == nil || *ls[0].MistakeKey != "chased" ||
		ls[0].Scope != "trade" || ls[0].Day != "2026-10-09" || ls[0].EntryID == nil || *ls[0].EntryID != entryID {
		t.Fatalf("lesson rows: %+v", ls)
	}

	// A re-review replaces the note's lesson.
	reply("- **Entry:** late.\n\n<!-- tj-ai:lesson key=chased | Let the first pullback come. -->\n<!-- tj-ai:verdict tag=bad -->")
	if rec := serve(ctx, mux, http.MethodPost, "/ai/entry/"+itoa64(entryID)); rec.Code != http.StatusOK {
		t.Fatalf("re-review = %d", rec.Code)
	}
	settle(t, h)
	if ls = lessons(); len(ls) != 1 || ls[0].Text != "Let the first pullback come." {
		t.Fatalf("re-review should replace the lesson: %+v", ls)
	}

	// The next review reads it; its own lesson is the same mistake again.
	reply("- **Entry:** chased again.\n\n<!-- tj-ai:lesson key=chased | Wait for the retest, again. -->\n<!-- tj-ai:verdict tag=bad -->")
	if rec := serve(ctx, mux, http.MethodPost, "/ai/trade/10"); rec.Code != http.StatusOK {
		t.Fatalf("second review = %d", rec.Code)
	}
	settle(t, h)
	fp.mu.Lock()
	user := fp.user
	fp.mu.Unlock()
	if !strings.Contains(user, `"recentLessons"`) || !strings.Contains(user, "Let the first pullback come.") {
		t.Errorf("the next review was not handed the recent lessons")
	}
	open := h.openLessons(ctx, "2000-01-01")
	if len(open) != 1 || open[0].Key != "chased" || open[0].Count != 2 || open[0].Name != "Chased the entry" {
		t.Fatalf("open lessons: %+v", open)
	}

	// The ledger page shows them; Learned on the mistake closes both.
	req := httptest.NewRequest(http.MethodGet, "/journal/lessons", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.LessonsPage(rec, req)
	page := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(page, "×2") || !strings.Contains(page, "Wait for the retest, again.") ||
		!strings.Contains(page, `href="/journal/trade/12?back=%2Fjournal%2Flessons"`) {
		t.Fatalf("lessons page = %d:\n%s", rec.Code, page)
	}

	// The week's digest lists them.
	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	d, err := h.buildDigest(ctx, monday, "2026-W41")
	if err != nil || len(d.Lessons) != 1 || d.Lessons[0].Count != 2 || d.Lessons[0].Open != 2 {
		t.Fatalf("digest lessons: %+v, %v", d, err)
	}
	if md := digestMarkdown(d); !strings.Contains(md, "## Lessons from the reviews") || !strings.Contains(md, "(2 times)") {
		t.Errorf("digest markdown lacks the lessons:\n%s", md)
	}

	post := httptest.NewRequest(http.MethodPost, "/journal/lessons/learned", strings.NewReader(url.Values{"key": {"chased"}}.Encode())).WithContext(ctx)
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.LessonsLearned(rec, post)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/journal/lessons?flash=learned" {
		t.Fatalf("learned = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if open := h.openLessons(ctx, "2000-01-01"); len(open) != 0 {
		t.Errorf("still open after Learned: %+v", open)
	}
	if d, _ := h.buildDigest(ctx, monday, "2026-W41"); len(d.Lessons) != 1 || d.Lessons[0].Open != 0 {
		t.Errorf("the digest should show the lessons as learned: %+v", d.Lessons)
	}
}
