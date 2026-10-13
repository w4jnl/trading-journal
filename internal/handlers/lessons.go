package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- the lessons ledger ----------------------------------------------------------
//
// Every in-app review names its single lesson in a machine line (stripped
// from the note) and the lesson is kept (lessons table): lessons about the
// same mistake are counted while open, shown on the day plan and the digest,
// drafted into the next plan and handed to the next review, which says
// whether this trade repeated one or applied it. The trader marks a lesson
// learned on /journal/lessons.

// aiLessonRe matches the lesson line, e.g.
//
//	<!-- tj-ai:lesson key=chased | Wait for the retest instead of buying the break. -->
var aiLessonRe = regexp.MustCompile(`(?is)<!--\s*tj-ai:lesson\s+(.*?)\s*-->`)

// aiLesson is a parsed lesson line.
type aiLesson struct {
	Key  string // a mistake label key, "" for none
	Text string
}

// parseAILesson reads the lesson line ("" text when there is none). The key
// must be in the mistake vocabulary; anything else reads as no key.
func parseAILesson(s string) aiLesson {
	m := aiLessonRe.FindStringSubmatch(s)
	if m == nil {
		return aiLesson{}
	}
	inner := strings.TrimSpace(m[1])
	var l aiLesson
	if strings.HasPrefix(strings.ToLower(inner), "key=") {
		rest := inner[4:]
		cut := strings.IndexAny(rest, "| \t\n")
		if cut < 0 {
			cut = len(rest)
		}
		if k := strings.ToLower(strings.TrimSpace(rest[:cut])); journal.ValidLabel(journal.KindMistake, k) && k != "none" {
			l.Key = k
		}
		inner = rest[cut:]
	}
	l.Text = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(inner), "|"))
	if len([]rune(l.Text)) > 400 {
		l.Text = string([]rune(l.Text)[:400])
	}
	return l
}

func stripAILesson(s string) string {
	return strings.TrimRight(aiLessonRe.ReplaceAllString(s, ""), " \t\n")
}

// aiLessonInstruction asks every review for the lesson line.
func aiLessonInstruction() string {
	return `Just before the verdict line, add EXACTLY one line naming the single most useful lesson (it is removed before saving and kept in the trader's lessons ledger):
<!-- tj-ai:lesson key=<` + strings.Join(journal.LabelKeys(journal.KindMistake), "|") + `|none> | <the lesson: one sentence, an instruction the trader can follow next time> -->
- key: the mistake the lesson is about, from that list, or none.`
}

// recordLesson replaces the entry's lesson with the review's.
func (h *Handlers) recordLesson(ctx context.Context, e models.JournalEntry, l aiLesson) {
	if l.Text == "" {
		return
	}
	uid := userID(ctx)
	if err := h.Queries.DeleteLessonsForEntry(ctx, models.DeleteLessonsForEntryParams{UserID: uid, EntryID: &e.ID}); err != nil {
		h.logger().Warn("drop lesson", "entry", e.ID, "err", err)
	}
	var key *string
	if l.Key != "" {
		key = &l.Key
	}
	id := e.ID
	if err := h.Queries.InsertLesson(ctx, models.InsertLessonParams{
		UserID: uid, EntryID: &id, Day: e.EntryDate.In(h.localTZ()).Format("2006-01-02"), Scope: e.Scope, MistakeKey: key, Text: l.Text,
	}); err != nil {
		h.logger().Warn("record lesson", "entry", e.ID, "err", err)
	}
}

// aiRecentLessons is the context a review gets: the trader's newest lessons
// from other notes.
func (h *Handlers) aiRecentLessons(ctx context.Context, entryID int64) []map[string]string {
	rows, err := h.Queries.ListRecentLessons(ctx, models.ListRecentLessonsParams{UserID: userID(ctx), EntryID: &entryID, Limit: 5})
	if err != nil {
		return nil
	}
	out := make([]map[string]string, 0, len(rows))
	for _, l := range rows {
		m := map[string]string{"day": l.Day, "lesson": l.Text}
		if l.MistakeKey != nil {
			m["mistake"] = *l.MistakeKey
		}
		if l.LearnedAt != nil {
			m["status"] = "marked learned"
		}
		out = append(out, m)
	}
	return out
}

// lessonGroup is a lesson as the plan, the digest and the ledger show it:
// lessons about one mistake together (counted), a lesson without one on its
// own.
type lessonGroup struct {
	Key      string   `json:"mistake,omitempty"` // the mistake key; "" for a single lesson without one
	Name     string   `json:"mistakeName,omitempty"`
	Count    int      `json:"count"`
	Open     int      `json:"open"`   // of them, not marked learned
	Text     string   `json:"lesson"` // the newest lesson's words
	Day      string   `json:"day"`    // its day
	LessonID int64    `json:"-"`      // the single lesson's id (Key == "")
	Texts    []string `json:"-"`      // up to 5 of the group's lessons, newest first
}

// openLessonDays is how far back a lesson still counts as open.
const openLessonDays = 30

// groupLessons groups lessons (newest first) by mistake, the most repeated
// first, then the newest.
func groupLessons(rows []models.Lesson) []lessonGroup {
	byKey := map[string]*lessonGroup{}
	var out []*lessonGroup
	for _, l := range rows {
		var g *lessonGroup
		if l.MistakeKey == nil {
			g = &lessonGroup{Text: l.Text, Day: l.Day, LessonID: l.ID}
			out = append(out, g)
		} else if g = byKey[*l.MistakeKey]; g == nil {
			g = &lessonGroup{Key: *l.MistakeKey, Name: journal.LabelName(journal.KindMistake, *l.MistakeKey), Text: l.Text, Day: l.Day}
			byKey[*l.MistakeKey] = g
			out = append(out, g)
		}
		g.Count++
		if l.LearnedAt == nil {
			g.Open++
		}
		if len(g.Texts) < 5 {
			g.Texts = append(g.Texts, l.Text)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Day > out[j].Day
	})
	res := make([]lessonGroup, len(out))
	for i, g := range out {
		res[i] = *g
	}
	return res
}

// openLessons groups the trader's open lessons since a day.
func (h *Handlers) openLessons(ctx context.Context, since string) []lessonGroup {
	rows, err := h.Queries.ListOpenLessons(ctx, models.ListOpenLessonsParams{UserID: userID(ctx), Day: since})
	if err != nil {
		h.logger().Warn("open lessons", "err", err)
		return nil
	}
	return groupLessons(rows)
}

// topOpenLessons is the few open lessons a plan or a digest leads with.
func (h *Handlers) topOpenLessons(ctx context.Context, n int) []lessonGroup {
	since := time.Now().In(h.localTZ()).AddDate(0, 0, -openLessonDays).Format("2006-01-02")
	all := h.openLessons(ctx, since)
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// lessonRow is a lesson in the ledger's list, with a link to its note.
type lessonRow struct {
	models.Lesson
	Href    string
	Mistake string // the mistake's name
}

// LessonsPage: GET /journal/lessons - the open lessons (repeated ones first)
// and the last 90 days of lessons.
func (h *Handlers) LessonsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().In(h.localTZ())
	rows, err := h.Queries.ListLessonsBetween(ctx, models.ListLessonsBetweenParams{
		UserID: userID(ctx), Day: now.AddDate(0, 0, -90).Format("2006-01-02"), Day_2: now.Format("2006-01-02"),
	})
	if err != nil {
		h.serverError(w, err)
		return
	}
	links := h.lessonLinks(ctx, rows)
	recent := make([]lessonRow, 0, len(rows))
	for _, l := range rows {
		lr := lessonRow{Lesson: l}
		if l.EntryID != nil {
			lr.Href = links[*l.EntryID]
		}
		if l.MistakeKey != nil {
			lr.Mistake = journal.LabelName(journal.KindMistake, *l.MistakeKey)
		}
		recent = append(recent, lr)
	}
	h.renderPage(w, r, "lessons.html", map[string]any{
		"NavActive": "journal",
		"BackHref":  validBackHref(r),
		"Open":      h.openLessons(ctx, now.AddDate(0, 0, -openLessonDays).Format("2006-01-02")),
		"Recent":    recent,
		"Flash":     r.URL.Query().Get("flash"),
	})
}

// lessonLinks maps the lessons' notes to their pages (back to the ledger).
func (h *Handlers) lessonLinks(ctx context.Context, rows []models.Lesson) map[int64]string {
	out := map[int64]string{}
	var ids []any
	seen := map[int64]bool{}
	for _, l := range rows {
		if l.EntryID != nil && !seen[*l.EntryID] {
			seen[*l.EntryID] = true
			ids = append(ids, *l.EntryID)
		}
	}
	if len(ids) == 0 {
		return out
	}
	q := `SELECT id, scope, trade_id, entry_date FROM journal_entries WHERE user_id = ? AND id IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
	res, err := h.rdb().QueryContext(ctx, q, append([]any{userID(ctx)}, ids...)...)
	if err != nil {
		h.logger().Warn("lesson links", "err", err)
		return out
	}
	defer res.Close()
	back := "?back=" + url.QueryEscape("/journal/lessons")
	tz := h.localTZ()
	for res.Next() {
		var (
			id      int64
			scope   string
			tradeID *int64
			date    time.Time
		)
		if res.Scan(&id, &scope, &tradeID, &date) != nil {
			continue
		}
		switch scope {
		case "trade":
			if tradeID != nil {
				out[id] = "/journal/trade/" + strconv.FormatInt(*tradeID, 10) + back
			}
		case "group":
			out[id] = "/journal/group/" + strconv.FormatInt(id, 10) + back
		case "week":
			y, wk := date.In(tz).ISOWeek()
			out[id] = fmt.Sprintf("/journal/week/%d-W%02d", y, wk) + back
		default:
			out[id] = "/journal/" + date.In(tz).Format("2006-01-02") + back
		}
	}
	return out
}

// LessonsLearned: POST /journal/lessons/learned - mark a mistake's open
// lessons (form key) or one lesson (form id) learned.
func (h *Handlers) LessonsLearned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var err error
	if key := r.PostFormValue("key"); key != "" {
		_, err = h.Queries.MarkLessonKeyLearned(ctx, models.MarkLessonKeyLearnedParams{UserID: userID(ctx), MistakeKey: &key})
	} else if id, perr := strconv.ParseInt(r.PostFormValue("id"), 10, 64); perr == nil {
		_, err = h.Queries.MarkLessonLearned(ctx, models.MarkLessonLearnedParams{ID: id, UserID: userID(ctx)})
	} else {
		err = errors.New("nothing to mark")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.LiveData.Bump(userID(ctx))
	back := "/journal/lessons?flash=learned"
	if b := r.PostFormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		back = b
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
