// Package handlers implements the HTTP layer for the trading journal UI.
//
// Pages render a base layout + page template via html/template. HTMX partials
// (like the import outcome panel) render single named templates without the
// base wrap. Handlers is the shared dependency container.
package handlers

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/jaro/trading-journal/internal/importer"
	"github.com/jaro/trading-journal/internal/models"
)

// Handlers groups every HTTP handler with its shared deps. main.go injects
// concrete values; tests can swap any field.
type Handlers struct {
	Templates  fs.FS // embedded template tree; pages live at templates/<name>
	ImportSvc  *importer.ImportService
	Queries    *models.Queries
	UploadsDir string // staging dir for UI uploads — must be on the same filesystem as ImportSvc.ProcessedDir
	Timezone   *time.Location
	Logger     *slog.Logger
}

// renderPage parses templates/base.html + templates/<page> together and
// executes the result. Re-parsing on every request is cheap with embed.FS
// and keeps the template wiring trivial; we can cache later if it ever shows up.
func (h *Handlers) renderPage(w http.ResponseWriter, page string, data any) {
	t, err := template.New("").Funcs(h.funcMap()).ParseFS(h.Templates, "templates/base.html", "templates/"+page)
	if err != nil {
		h.serverError(w, fmt.Errorf("parse %s: %w", page, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base.html", data); err != nil {
		h.logger().Error("render", "page", page, "err", err)
	}
}

// renderPartial renders a single named template — no base wrap. Used for
// HTMX fragment responses.
func (h *Handlers) renderPartial(w http.ResponseWriter, partial, name string, data any) {
	t, err := template.New("").Funcs(h.funcMap()).ParseFS(h.Templates, "templates/"+partial)
	if err != nil {
		h.serverError(w, fmt.Errorf("parse %s: %w", partial, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		h.logger().Error("render partial", "name", name, "err", err)
	}
}

func (h *Handlers) funcMap() template.FuncMap {
	loc := h.Timezone
	if loc == nil {
		loc = time.UTC
	}
	return template.FuncMap{
		"derefStr": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		"localtime": func(t time.Time, layout string) string {
			return t.In(loc).Format(layout)
		},
	}
}

func (h *Handlers) serverError(w http.ResponseWriter, err error) {
	h.logger().Error("server error", "err", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (h *Handlers) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}
