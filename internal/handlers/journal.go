package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
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

	flash := r.URL.Query().Get("flash")

	h.renderPage(w, "journal.html", map[string]any{
		"Date":      dateStr,
		"DateNice":  date.Format("Mon, 2 Jan 2006"),
		"PrevDay":   prevDay,
		"NextDay":   nextDay,
		"Today":     time.Now().In(h.localTZ()).Format("2006-01-02"),
		"HasEntry":  hasEntry,
		"Entry":     entry,
		"Mood":      mood,
		"Rendered":  rendered,
		"Recent":    recent,
		"Flash":     flash,
	})
}

// JournalSave handles POST /journal/{date}: upserts a day-level entry for
// the date, then redirects back to GET /journal/{date} so a refresh doesn't
// resubmit the form.
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

	body := strings.TrimSpace(r.FormValue("body"))
	moodStr := strings.TrimSpace(r.FormValue("mood"))
	var mood *int64
	if moodStr != "" {
		if m, err := strconv.ParseInt(moodStr, 10, 64); err == nil && m >= 1 && m <= 5 {
			mood = &m
		}
	}

	existing, err := h.Queries.GetDayJournalEntry(ctx, date)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if body == "" {
			// Nothing to save. Just redirect.
			http.Redirect(w, r, "/journal/"+dateStr+"?flash=empty", http.StatusSeeOther)
			return
		}
		if _, err := h.Queries.CreateJournalEntry(ctx, models.CreateJournalEntryParams{
			TradeID:   nil,
			AccountID: nil,
			EntryDate: date,
			Body:      body,
			Mood:      mood,
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
			http.Redirect(w, r, "/journal/"+dateStr+"?flash=deleted", http.StatusSeeOther)
			return
		}
		if err := h.Queries.UpdateJournalEntry(ctx, models.UpdateJournalEntryParams{
			Body: body,
			Mood: mood,
			ID:   existing.ID,
		}); err != nil {
			h.serverError(w, fmt.Errorf("update journal: %w", err))
			return
		}
	}

	http.Redirect(w, r, "/journal/"+dateStr+"?flash=saved", http.StatusSeeOther)
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
