// Package handlers implements the HTTP layer for the trading journal UI.
//
// Pages render a base layout + page template via html/template. HTMX partials
// (like the import outcome panel) render single named templates without the
// base wrap. Handlers is the shared dependency container.
package handlers

import (
	"database/sql"
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
	DB         *sql.DB // raw connection for hand-written dynamic queries
	ImportSvc  *importer.ImportService
	Queries    *models.Queries
	UploadsDir string // staging dir for UI uploads — must be on the same filesystem as ImportSvc.ProcessedDir
	Timezone   *time.Location
	Logger     *slog.Logger
}

// renderPage parses base.html + the named page + every _*.html partial in the
// templates dir, and executes the base. Bundling the partials means a page
// can reference any sibling partial via {{template "name" .}} without the
// caller listing it explicitly.
func (h *Handlers) renderPage(w http.ResponseWriter, page string, data any) {
	files := []string{"templates/base.html", "templates/" + page}
	partials, err := fs.Glob(h.Templates, "templates/_*.html")
	if err != nil {
		h.serverError(w, fmt.Errorf("glob partials: %w", err))
		return
	}
	files = append(files, partials...)

	t, err := template.New("").Funcs(h.funcMap()).ParseFS(h.Templates, files...)
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
		"deref": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"localtime": func(t time.Time, layout string) string {
			return t.In(loc).Format(layout)
		},
		// pnlClass renders a *float64 with a color span based on sign.
		// Nil values render as a plain em-dash; the position builder writes
		// NULL for trades whose instrument has no tick spec configured yet.
		"pnlClass": func(p *float64) template.HTML {
			if p == nil {
				return template.HTML("—")
			}
			cls := "pnl-pos"
			if *p < 0 {
				cls = "pnl-neg"
			}
			return template.HTML(fmt.Sprintf(`<span class="%s">%.2f</span>`, cls, *p))
		},
		// dict builds an ad-hoc map from key/value pairs. Useful for passing
		// multi-field data into a sub-template via {{template "x" (dict ...)}}.
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			out := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d not a string", i/2)
				}
				out[k] = pairs[i+1]
			}
			return out, nil
		},
		// mkRange returns []int64{from, from+1, …, to} for use in {{range}}.
		"mkRange": func(from, to int64) []int64 {
			if to < from {
				return nil
			}
			out := make([]int64, 0, to-from+1)
			for i := from; i <= to; i++ {
				out = append(out, i)
			}
			return out
		},
		"moodEmoji": func(mood int64) string {
			switch mood {
			case 1:
				return "😞"
			case 2:
				return "😕"
			case 3:
				return "😐"
			case 4:
				return "🙂"
			case 5:
				return "😀"
			}
			return ""
		},
		// snippet truncates s to at most n runes, appending "…" if cut.
		"snippet": func(s string, n int) string {
			r := []rune(s)
			if len(r) <= n {
				return s
			}
			return string(r[:n]) + "…"
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
