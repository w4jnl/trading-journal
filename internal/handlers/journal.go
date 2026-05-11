package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gomarkdown/markdown"
	mdHtml "github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"

	"github.com/jaro/trading-journal/internal/models"
)

// JournalFeed renders /journal — the chronological feed mixing day, trade,
// and week entries by date desc. Quick-add bar at the top lets the user jump
// to today / any date / any week. Clicking a feed item opens the right edit
// surface (modal for trade scope, page for day/week scopes).
func (h *Handlers) JournalFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()

	rows, err := h.Queries.ListJournalFeed(ctx, 100)
	if err != nil {
		h.serverError(w, fmt.Errorf("list feed: %w", err))
		return
	}

	type feedItem struct {
		ID           int64
		Scope        string  // day | trade | week
		Date         time.Time
		DateLabel    string
		MetaLabel    string  // "APEX-1 / MESM6 long · $42.50" for trade, "May 4–10" for week
		Tag          string
		Body         string
		EditURL      string  // where to navigate when the row is clicked
	}
	items := make([]feedItem, 0, len(rows))
	for _, row := range rows {
		fi := feedItem{
			ID:    row.ID,
			Scope: row.Scope,
			Date:  row.EntryDate.In(tz),
			Body:  row.Body,
		}
		if row.Tag != nil {
			fi.Tag = *row.Tag
		}
		switch row.Scope {
		case "day":
			fi.DateLabel = fi.Date.Format("Mon, 2 Jan 2006")
			fi.EditURL = "/journal/" + fi.Date.Format("2006-01-02") + "?back=/journal"
		case "trade":
			fi.DateLabel = fi.Date.Format("Mon, 2 Jan 2006")
			meta := ""
			if row.AccountName != nil {
				meta = *row.AccountName
			}
			if row.TradeSymbol != nil {
				if meta != "" {
					meta += " · "
				}
				meta += *row.TradeSymbol
				if row.TradeDirection != nil {
					meta += " " + *row.TradeDirection
				}
			}
			if row.TradeNetPnl != nil {
				if meta != "" {
					meta += " · "
				}
				meta += fmt.Sprintf("$%.2f", *row.TradeNetPnl)
			}
			fi.MetaLabel = meta
			if row.TradeID != nil {
				fi.EditURL = fmt.Sprintf("/trades?modal=%d&back=/journal", *row.TradeID)
			}
		case "week":
			y, w := fi.Date.ISOWeek()
			sunday := fi.Date.AddDate(0, 0, 6)
			fi.DateLabel = fmt.Sprintf("Week %d", w)
			fi.MetaLabel = fmt.Sprintf("%s – %s", fi.Date.Format("Jan 2"), sunday.Format("Jan 2"))
			fi.EditURL = fmt.Sprintf("/journal/week/%d-W%02d?back=/journal", y, w)
		}
		items = append(items, fi)
	}

	now := time.Now().In(tz)
	todayStr := now.Format("2006-01-02")
	y, wk := now.ISOWeek()
	thisWeekStr := fmt.Sprintf("%d-W%02d", y, wk)

	h.renderPage(w, "journal_feed.html", map[string]any{
		"NavActive":   "journal",
		"BackHref":    validBackHref(r),
		"Items":       items,
		"Today":       todayStr,
		"ThisWeek":    thisWeekStr,
	})
}

// JournalPage renders /journal/{date} — the day-level entry editor for a
// specific YYYY-MM-DD.
func (h *Handlers) JournalPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dateStr := r.PathValue("date")
	if dateStr == "" {
		dateStr = time.Now().In(h.localTZ()).Format("2006-01-02")
	}
	date, err := time.ParseInLocation("2006-01-02", dateStr, h.localTZ())
	if err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}

	entry, err := h.Queries.GetDayJournalEntry(ctx, date)
	hasEntry := true
	if errors.Is(err, sql.ErrNoRows) {
		hasEntry = false
		entry = models.JournalEntry{}
	} else if err != nil {
		h.serverError(w, fmt.Errorf("get day entry: %w", err))
		return
	}

	recent, err := h.Queries.ListRecentJournalEntries(ctx, 30)
	if err != nil {
		h.serverError(w, fmt.Errorf("list recent: %w", err))
		return
	}

	prevDay := date.AddDate(0, 0, -1).Format("2006-01-02")
	nextDay := date.AddDate(0, 0, 1).Format("2006-01-02")

	var rendered template.HTML
	if hasEntry && entry.Body != "" {
		rendered = template.HTML(renderMarkdown(entry.Body))
	}

	mood := int64(0)
	if entry.Mood != nil {
		mood = *entry.Mood
	}
	tag := ""
	if entry.Tag != nil {
		tag = *entry.Tag
	}

	flash := r.URL.Query().Get("flash")

	// Cross-page links into this view set ?back=<relative-url>. Falls back to
	// the legacy ?from=calendar form for any in-flight tabs from the older
	// nav scheme.
	backHref := validBackHref(r)
	if backHref == "" && r.URL.Query().Get("from") == "calendar" {
		q := url.Values{}
		if v := r.URL.Query().Get("month"); v != "" {
			q.Set("month", v)
		}
		if v := r.URL.Query().Get("account_id"); v != "" {
			q.Set("account_id", v)
		}
		backHref = "/calendar"
		if encoded := q.Encode(); encoded != "" {
			backHref += "?" + encoded
		}
	}

	h.renderPage(w, "journal.html", map[string]any{
		"NavActive": "journal",
		"BackHref":  backHref,
		"Date":      dateStr,
		"DateNice":  date.Format("Mon, 2 Jan 2006"),
		"PrevDay":   prevDay,
		"NextDay":   nextDay,
		"Today":     time.Now().In(h.localTZ()).Format("2006-01-02"),
		"HasEntry":  hasEntry,
		"Entry":     entry,
		"Mood":      mood,
		"Tag":       tag,
		"Rendered":  rendered,
		"Recent":    recent,
		"Flash":     flash,
	})
}

// parseMood reads the form value 1-5 for mood, returning nil for invalid/empty.
func parseMood(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m, err := strconv.ParseInt(s, 10, 64)
	if err != nil || m < 1 || m > 5 {
		return nil
	}
	return &m
}

// parseTag validates the quick-classify tag — good/bad/neutral or nil.
func parseTag(s string) *string {
	s = strings.TrimSpace(s)
	switch s {
	case "good", "bad", "neutral":
		return &s
	}
	return nil
}

// JournalSave handles POST /journal/{date}: upserts a day-level entry for
// the date, then redirects back to GET /journal/{date} so a refresh doesn't
// resubmit the form. Carries through the `back` field if set, so the
// "Back to calendar" round-trip survives the save.
func (h *Handlers) JournalSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dateStr := r.PathValue("date")
	date, err := time.ParseInLocation("2006-01-02", dateStr, h.localTZ())
	if err != nil {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	// Reject future-dated journal entries — UI hides the link too, but defend
	// the API path against direct POSTs.
	now := time.Now().In(h.localTZ())
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, h.localTZ())
	if date.After(today) {
		http.Error(w, "journal entries are not allowed on future dates", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	redirectQuery := func(flash string) string {
		q := url.Values{}
		q.Set("flash", flash)
		// Round-trip the back URL if the form carried it. Also accepts legacy
		// from/month/account_id triple for in-flight tabs.
		if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
			q.Set("back", v)
		}
		for _, key := range []string{"from", "month", "account_id"} {
			if v := r.FormValue(key); v != "" {
				q.Set(key, v)
			}
		}
		return "/journal/" + dateStr + "?" + q.Encode()
	}

	body := strings.TrimSpace(r.FormValue("body"))
	mood := parseMood(r.FormValue("mood"))
	tag := parseTag(r.FormValue("tag"))

	existing, err := h.Queries.GetDayJournalEntry(ctx, date)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if body == "" {
			// Nothing to save. Just redirect.
			http.Redirect(w, r, redirectQuery("empty"), http.StatusSeeOther)
			return
		}
		if _, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			TradeID:   nil,
			AccountID: nil,
			EntryDate: date,
			Body:      body,
			Mood:      mood,
			Tag:       tag,
			Scope:     "day",
		}); err != nil {
			h.serverError(w, fmt.Errorf("create journal: %w", err))
			return
		}
	case err != nil:
		h.serverError(w, fmt.Errorf("get journal: %w", err))
		return
	default:
		if body == "" {
			// Empty save = delete the entry.
			if err := h.Queries.DeleteJournalEntry(ctx, existing.ID); err != nil {
				h.serverError(w, fmt.Errorf("delete journal: %w", err))
				return
			}
			http.Redirect(w, r, redirectQuery("deleted"), http.StatusSeeOther)
			return
		}
		if err := h.Queries.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
			Body: body,
			Mood: mood,
			Tag:  tag,
			ID:   existing.ID,
		}); err != nil {
			h.serverError(w, fmt.Errorf("update journal: %w", err))
			return
		}
	}

	http.Redirect(w, r, redirectQuery("saved"), http.StatusSeeOther)
}

// WeekJournalPage renders /journal/week/{2026-W19} — the week-level entry
// editor. entry_date for a week-scoped row is the Monday of that ISO week.
func (h *Handlers) WeekJournalPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	weekID := r.PathValue("week")
	monday, isoYear, isoWeek, err := parseISOWeek(weekID, h.localTZ())
	if err != nil {
		http.Error(w, "bad week id: expected YYYY-Www (e.g. 2026-W19)", http.StatusBadRequest)
		return
	}

	entry, err := h.Queries.GetWeekJournalEntry(ctx, monday)
	hasEntry := true
	if errors.Is(err, sql.ErrNoRows) {
		hasEntry = false
		entry = models.JournalEntry{}
	} else if err != nil {
		h.serverError(w, fmt.Errorf("get week entry: %w", err))
		return
	}

	var rendered template.HTML
	if hasEntry && entry.Body != "" {
		rendered = template.HTML(renderMarkdown(entry.Body))
	}
	tag := ""
	if entry.Tag != nil {
		tag = *entry.Tag
	}

	prev := monday.AddDate(0, 0, -7)
	next := monday.AddDate(0, 0, 7)
	py, pw := prev.ISOWeek()
	ny, nw := next.ISOWeek()
	sunday := monday.AddDate(0, 0, 6)

	h.renderPage(w, "journal_week.html", map[string]any{
		"NavActive":  "journal",
		"BackHref":   validBackHref(r),
		"WeekID":     fmt.Sprintf("%d-W%02d", isoYear, isoWeek),
		"WeekLabel":  fmt.Sprintf("Week %d, %d", isoWeek, isoYear),
		"RangeLabel": fmt.Sprintf("%s – %s", monday.Format("Mon, 2 Jan"), sunday.Format("Mon, 2 Jan 2006")),
		"PrevWeek":   fmt.Sprintf("%d-W%02d", py, pw),
		"NextWeek":   fmt.Sprintf("%d-W%02d", ny, nw),
		"HasEntry":   hasEntry,
		"Entry":      entry,
		"Tag":        tag,
		"Rendered":   rendered,
		"Flash":      r.URL.Query().Get("flash"),
	})
}

// WeekJournalSave handles POST /journal/week/{week_id}.
func (h *Handlers) WeekJournalSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	weekID := r.PathValue("week")
	monday, _, _, err := parseISOWeek(weekID, h.localTZ())
	if err != nil {
		http.Error(w, "bad week id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	redirectQuery := func(flash string) string {
		q := url.Values{}
		q.Set("flash", flash)
		if v := r.FormValue("back"); v != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
			q.Set("back", v)
		}
		return "/journal/week/" + weekID + "?" + q.Encode()
	}

	body := strings.TrimSpace(r.FormValue("body"))
	tag := parseTag(r.FormValue("tag"))

	existing, err := h.Queries.GetWeekJournalEntry(ctx, monday)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if body == "" {
			http.Redirect(w, r, redirectQuery("empty"), http.StatusSeeOther)
			return
		}
		if _, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			TradeID:   nil,
			AccountID: nil,
			EntryDate: monday,
			Body:      body,
			Mood:      nil,
			Tag:       tag,
			Scope:     "week",
		}); err != nil {
			h.serverError(w, fmt.Errorf("create week journal: %w", err))
			return
		}
	case err != nil:
		h.serverError(w, fmt.Errorf("get week journal: %w", err))
		return
	default:
		if body == "" {
			if err := h.Queries.DeleteJournalEntry(ctx, existing.ID); err != nil {
				h.serverError(w, fmt.Errorf("delete week journal: %w", err))
				return
			}
			http.Redirect(w, r, redirectQuery("deleted"), http.StatusSeeOther)
			return
		}
		if err := h.Queries.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
			Body: body,
			Mood: existing.Mood,
			Tag:  tag,
			ID:   existing.ID,
		}); err != nil {
			h.serverError(w, fmt.Errorf("update week journal: %w", err))
			return
		}
	}
	http.Redirect(w, r, redirectQuery("saved"), http.StatusSeeOther)
}

// parseISOWeek parses "YYYY-Www" and returns the Monday of that ISO week
// (in tz, at midnight), plus the ISO year/week components for re-formatting.
// The algorithm anchors on Jan 4 — which is always in ISO week 1 — then walks
// back to that week's Monday and forward N-1 weeks.
func parseISOWeek(s string, tz *time.Location) (monday time.Time, isoYear, isoWeek int, err error) {
	parts := strings.SplitN(s, "-W", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, 0, fmt.Errorf("invalid week id %q", s)
	}
	year, err1 := strconv.Atoi(parts[0])
	week, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || week < 1 || week > 53 {
		return time.Time{}, 0, 0, fmt.Errorf("invalid week id %q", s)
	}
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, tz)
	wd := int(jan4.Weekday())
	if wd == 0 {
		wd = 7
	}
	week1Monday := jan4.AddDate(0, 0, 1-wd)
	monday = week1Monday.AddDate(0, 0, (week-1)*7)
	y, w := monday.ISOWeek()
	return monday, y, w, nil
}

// renderMarkdown converts a markdown string into safe HTML. Uses gomarkdown
// with default extensions (CommonMark + tables, fenced code blocks). The
// output is sanitized via html/template's auto-escape further up — but we
// trust gomarkdown's HTML output here because the input is the user's own
// journal content (single-user app).
func renderMarkdown(src string) string {
	p := parser.NewWithExtensions(parser.CommonExtensions | parser.AutoHeadingIDs)
	doc := p.Parse([]byte(src))
	renderer := mdHtml.NewRenderer(mdHtml.RendererOptions{
		Flags: mdHtml.CommonFlags | mdHtml.HrefTargetBlank,
	})
	return string(markdown.Render(doc, renderer))
}

func (h *Handlers) localTZ() *time.Location {
	if h.Timezone != nil {
		return h.Timezone
	}
	return time.UTC
}
