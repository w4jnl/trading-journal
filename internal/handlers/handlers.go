// Package handlers implements the HTTP layer for the trading journal UI.
//
// Pages render a base layout + page template via html/template. HTMX partials
// (like the import outcome panel) render single named templates without the
// base wrap. Handlers is the shared dependency container.
package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Handlers groups every HTTP handler with its shared deps. main.go injects
// concrete values; tests can swap any field.
type Handlers struct {
	Templates      fs.FS // embedded template tree; pages live at templates/<name>
	DB             *sql.DB // raw connection for hand-written dynamic queries
	ImportSvc      *importer.ImportService
	Queries        *models.Queries
	UploadsDir     string // staging dir for UI uploads — must be on the same filesystem as ImportSvc.ProcessedDir
	AttachmentsDir string // where journal-attached images are stored on disk
	Timezone       *time.Location
	Logger         *slog.Logger

	// templateCache memoizes parsed template sets so renderPage / renderPartial
	// don't re-glob and re-parse the templates fs on every request. Keys
	// distinguish page renders ("page:<name>") from partial renders
	// ("partial:<name>") since the file sets differ.
	templateCache sync.Map
}

// renderPage parses base.html + the named page + every _*.html partial in the
// templates dir, and executes the base. Bundling the partials means a page
// can reference any sibling partial via {{template "name" .}} without the
// caller listing it explicitly. The parsed set is cached after first use —
// templates ship in embed.FS so they never change at runtime.
func (h *Handlers) renderPage(w http.ResponseWriter, page string, data any) {
	t, err := h.parsedPage(page)
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
// HTMX fragment responses. We bundle every `_*.html` partial (same as
// renderPage) so a partial can reference any shared sub-template
// (tag_picker, journal_editor, sketch icons, etc.) without listing them.
func (h *Handlers) renderPartial(w http.ResponseWriter, partial, name string, data any) {
	t, err := h.parsedPartial(partial)
	if err != nil {
		h.serverError(w, fmt.Errorf("parse %s: %w", partial, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		h.logger().Error("render partial", "name", name, "err", err)
	}
}

// parsedPage returns the cached template set for a page render (base + page
// + every partial). First call for a given page parses; subsequent calls hit
// the cache.
func (h *Handlers) parsedPage(page string) (*template.Template, error) {
	key := "page:" + page
	if v, ok := h.templateCache.Load(key); ok {
		return v.(*template.Template), nil
	}
	partials, err := fs.Glob(h.Templates, "templates/_*.html")
	if err != nil {
		return nil, fmt.Errorf("glob partials: %w", err)
	}
	files := append([]string{"templates/base.html", "templates/" + page}, partials...)
	t, err := template.New("").Funcs(h.funcMap()).ParseFS(h.Templates, files...)
	if err != nil {
		return nil, err
	}
	actual, _ := h.templateCache.LoadOrStore(key, t)
	return actual.(*template.Template), nil
}

// parsedPartial returns the cached template set for a partial render (the
// requested partial + every other partial as siblings).
func (h *Handlers) parsedPartial(partial string) (*template.Template, error) {
	key := "partial:" + partial
	if v, ok := h.templateCache.Load(key); ok {
		return v.(*template.Template), nil
	}
	files := []string{"templates/" + partial}
	siblings, err := fs.Glob(h.Templates, "templates/_*.html")
	if err != nil {
		return nil, fmt.Errorf("glob partials: %w", err)
	}
	for _, sib := range siblings {
		if sib == "templates/"+partial {
			continue
		}
		files = append(files, sib)
	}
	t, err := template.New("").Funcs(h.funcMap()).ParseFS(h.Templates, files...)
	if err != nil {
		return nil, err
	}
	actual, _ := h.templateCache.LoadOrStore(key, t)
	return actual.(*template.Template), nil
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
		// sparkPolyline scales a []float64 into the `points="x,y x,y …"` string
		// for an inline <svg><polyline></svg> sparkline. Coordinates fit a
		// fixed viewBox (default 60×20). Min-max normalized per series so
		// small absolute moves still render with full vertical range. Empty
		// or single-point series return an empty string (template `{{if}}` can
		// then suppress the SVG cleanly).
		"sparkPolyline": func(ys []float64) string {
			if len(ys) < 2 {
				return ""
			}
			const w, hgt = 60.0, 20.0
			min, max := ys[0], ys[0]
			for _, v := range ys {
				if v < min {
					min = v
				}
				if v > max {
					max = v
				}
			}
			span := max - min
			if span == 0 {
				span = 1 // flat line drawn at mid-height
			}
			step := w / float64(len(ys)-1)
			var b strings.Builder
			for i, v := range ys {
				x := float64(i) * step
				// Invert Y so larger values render higher; 1px padding inset
				// keeps the stroke from clipping at the viewbox edge.
				y := hgt - 1 - ((v-min)/span)*(hgt-2)
				if i > 0 {
					b.WriteByte(' ')
				}
				fmt.Fprintf(&b, "%.1f,%.1f", x, y)
			}
			return b.String()
		},
		// pnlChip renders a tinted-background P&L chip with an explicit +/−
		// sign so winners and losers pop in long lists. Use on the Net P&L
		// column in tables; pnlClass is still right for inline text.
		"pnlChip": func(p *float64) template.HTML {
			if p == nil {
				return template.HTML(`<span class="muted">—</span>`)
			}
			v := *p
			cls := "pnl-chip--soft-pos"
			sign := "+"
			if v < 0 {
				cls = "pnl-chip--soft-neg"
				sign = "−"
				v = -v
			}
			return template.HTML(fmt.Sprintf(`<span class="pnl-chip %s">%s%.2f</span>`, cls, sign, v))
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
		// cellClass picks a calendar-cell color class based on net P&L magnitude.
		// Threshold is rough: $100 / $-100 separates "regular day" from "big day".
		"cellClass": func(netPnl float64, hasData bool) string {
			if !hasData {
				return ""
			}
			switch {
			case netPnl >= 100:
				return "has-pnl-pos-strong"
			case netPnl > 0:
				return "has-pnl-pos"
			case netPnl <= -100:
				return "has-pnl-neg-strong"
			case netPnl < 0:
				return "has-pnl-neg"
			}
			return ""
		},
		"mul":         func(a, b float64) float64 { return a * b },
		"div":         func(a, b float64) float64 { if b == 0 { return 0 }; return a / b },
		"abs":         func(f float64) float64 { if f < 0 { return -f }; return f },
		"i64ToFloat":  func(i int64) float64 { return float64(i) },
		"i64ToInt":    func(i int64) int { return int(i) },
		"intToFloat":  func(i int) float64 { return float64(i) },
		"negFloat":    func(f float64) float64 { return -f },
		"isInf":       func(f float64) bool { return math.IsInf(f, 0) },
		// barWidth returns 0..100 — the percentage to use as a CSS bar width.
		// Used by the horizontal bar charts (Duration / Win Rate Analysis).
		"barWidth": func(v, max float64) float64 {
			if max <= 0 {
				return 0
			}
			pct := v / max * 100
			if pct < 0 {
				return 0
			}
			if pct > 100 {
				return 100
			}
			return pct
		},
		// humanDuration formats a time.Duration as "X min Y sec" / "X hr Y min"
		// for the dashboard tiles. Compact ASCII, no fractional seconds.
		"humanDuration": func(d time.Duration) string {
			if d <= 0 {
				return "—"
			}
			s := int(d.Seconds())
			if s < 60 {
				return fmt.Sprintf("%d sec", s)
			}
			if s < 3600 {
				return fmt.Sprintf("%d min %d sec", s/60, s%60)
			}
			h := s / 3600
			m := (s % 3600) / 60
			return fmt.Sprintf("%d hr %d min", h, m)
		},
		// journalState maps (hasEntry, tag) onto the journal_icon template's
		// state string: empty / neutral / good / bad. Untagged entries fall
		// back to "neutral" so they still get a distinct color from "empty".
		"journalState": func(has bool, tag string) string {
			if !has {
				return "empty"
			}
			switch tag {
			case "good", "bad", "neutral":
				return tag
			}
			return "neutral"
		},
		// weekCellClass tints a Saturday cell by the *week* net P&L. Threshold
		// is $500 vs $100 for daily — weekly aggregates are naturally bigger.
		"weekCellClass": func(netPnl float64, hasData bool) string {
			if !hasData {
				return ""
			}
			switch {
			case netPnl >= 500:
				return "week-pos-strong"
			case netPnl > 0:
				return "week-pos"
			case netPnl <= -500:
				return "week-neg-strong"
			case netPnl < 0:
				return "week-neg"
			}
			return ""
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

// validBackHref extracts and validates a ?back= URL from the request. The
// returned string is safe to use as an href: relative path starting with /
// (so a malicious caller can't redirect to an external site).
func validBackHref(r *http.Request) string {
	s := r.URL.Query().Get("back")
	if s == "" {
		return ""
	}
	// Only allow same-origin relative URLs. Reject "//" (protocol-relative)
	// and anything not starting with "/" — that excludes "http://…" too.
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return ""
	}
	return s
}

// retiredAccountFilterSQL is the bare WHERE fragment that excludes trades from
// retired accounts (accounts.active = 0). Use as `account_id IN (` + this + `)`
// or join with " AND " to existing WHERE clauses. Kept as a constant so the
// retirement rule lives in one place across dashboard/calendar/trades/analytics.
const retiredAccountFilterSQL = "account_id IN (SELECT id FROM accounts WHERE active = 1)"

// accountsForFilter returns the slice rendered in the account-filter dropdown
// on /trades, /calendar, /analytics. When includeRetired is false, retired
// accounts (active = 0) are dropped so the dropdown doesn't even hint at
// them — matches the contract that retired accounts disappear from every
// non-/trades view, and from /trades unless the reveal toggle is on.
func accountsForFilter(accounts []models.Account, includeRetired bool) []models.Account {
	if includeRetired {
		return accounts
	}
	out := make([]models.Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Active != 1 {
			continue
		}
		out = append(out, a)
	}
	return out
}

// retiredAccountSet returns the IDs of every retired account (active = 0)
// as a set for cheap membership tests. Backed by the sqlc-generated
// ListRetiredAccountIDs query — callers use the set to skip retired rows
// in already-fetched daily_stats or accounts lists without a re-query.
func (h *Handlers) retiredAccountSet(ctx context.Context) (map[int64]bool, error) {
	ids, err := h.Queries.ListRetiredAccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// accountFilter holds the parsed multi-select filter shared by /trades,
// /calendar, and /analytics. The `filter` query param accepts multiple values:
//   acc:<id>      — include this exact account
//   type:<slug>   — include every account whose `type` matches
// The resolved AccountIDs is the OR-union of both kinds. Active is true iff
// the user selected anything, even if the union is empty (then "show none").
// RawValues is the original list of selections, for re-rendering the
// dropdown's selected state. SelectedCount is len(non-empty selections).
type accountFilter struct {
	AccountIDs    []int64
	Active        bool
	RawValues     []string
	Selected      map[string]bool
	SelectedCount int
}

// resolveAccountFilter parses ?filter=acc:N&filter=type:X (multi-valued) into
// a concrete []int64 of accounts to filter by, resolving type slugs against
// the accounts table.
func (h *Handlers) resolveAccountFilter(r *http.Request) (accountFilter, error) {
	raw := r.URL.Query()["filter"]
	af := accountFilter{RawValues: raw, Selected: map[string]bool{}}
	if len(raw) == 0 {
		return af, nil
	}
	var explicitIDs []int64
	var types []string
	for _, v := range raw {
		if v == "" {
			continue
		}
		af.Selected[v] = true
		af.SelectedCount++
		switch {
		case strings.HasPrefix(v, "acc:"):
			if id, err := strconv.ParseInt(v[4:], 10, 64); err == nil {
				explicitIDs = append(explicitIDs, id)
			}
		case strings.HasPrefix(v, "type:"):
			types = append(types, v[5:])
		}
	}
	if len(explicitIDs) == 0 && len(types) == 0 {
		return af, nil
	}
	af.Active = true
	set := map[int64]bool{}
	for _, id := range explicitIDs {
		set[id] = true
	}
	if len(types) > 0 {
		placeholders := strings.Repeat("?,", len(types))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(types))
		for i, t := range types {
			args[i] = t
		}
		q := "SELECT id FROM accounts WHERE type IN (" + placeholders + ")"
		rows, err := h.DB.QueryContext(r.Context(), q, args...)
		if err != nil {
			return af, fmt.Errorf("resolve type filter: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return af, err
			}
			set[id] = true
		}
		if err := rows.Err(); err != nil {
			return af, err
		}
	}
	for id := range set {
		af.AccountIDs = append(af.AccountIDs, id)
	}
	return af, nil
}
