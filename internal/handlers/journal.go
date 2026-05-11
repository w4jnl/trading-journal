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

// JournalPage renders /journal — the day-level entry editor for a given date
// (defaults to today in trading-tz). Path /journal/{date} navigates to a
// specific YYYY-MM-DD; the recent-entries sidebar comes from sync_log of
// previously-saved day entries.
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
