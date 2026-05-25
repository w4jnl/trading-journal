package handlers

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Daily Open Interest dashboards (HTML + PDF pairs) live on disk under
// Handlers.OIDashboardsDir, named "dashboard-YYYY-MM-DD.{html,pdf}".
// They're produced by an external pipeline; we just list and serve.
//
// Filename convention is the source of truth for "which dashboards
// exist" - we don't recurse, we don't look at file mtime (so the same
// dashboard regenerated mid-day doesn't reorder).

const dashboardFilePrefix = "dashboard-"

// dashboardDateRE matches a YYYY-MM-DD path segment. Used to validate
// the {date} URL parameter so path-traversal attempts can't escape the
// configured root.
var dashboardDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// OIDashboardPage renders /oi and /oi/{date}. If {date} is empty we
// pick the latest available dashboard; if no dashboards exist, the
// template shows an empty-state panel.
func (h *Handlers) OIDashboardPage(w http.ResponseWriter, r *http.Request) {
	root := h.OIDashboardsDir
	dates, err := listDashboardDates(root)
	if err != nil {
		h.serverError(w, fmt.Errorf("list oi dashboards: %w", err))
		return
	}

	requested := strings.TrimSpace(r.PathValue("date"))
	if requested != "" && !dashboardDateRE.MatchString(requested) {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}

	selected := ""
	if len(dates) > 0 {
		selected = dates[0] // newest by default
	}
	if requested != "" {
		// Honour explicit selection iff the file is actually present;
		// otherwise fall through to the latest so a stale bookmark
		// doesn't dead-end on a missing file.
		for _, d := range dates {
			if d == requested {
				selected = requested
				break
			}
		}
	}

	hasPDF := selected != "" && fileExists(filepath.Join(root, dashboardFilePrefix+selected+".pdf"))

	h.renderPage(w, r, "oi_dashboard.html", map[string]any{
		"NavActive": "oi",
		"BackHref":  validBackHref(r),
		"Selected":  selected,
		"All":       dates,
		"HTMLPath":  fileURL(selected, "html"),
		"PDFPath":   fileURL(selected, "pdf"),
		"HasPDF":    hasPDF,
		"Empty":     selected == "",
		"Root":      root, // shown in empty-state so the user sees where to drop files
	})
}

// OIDashboardFile serves the raw dashboard file. The route is
// /oi/file/{name} where {name} is "YYYY-MM-DD.html" or
// "YYYY-MM-DD.pdf". The handler validates both halves and uses
// http.ServeFile under the configured root.
func (h *Handlers) OIDashboardFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		http.Error(w, "bad file", http.StatusBadRequest)
		return
	}
	date, ext := name[:dot], strings.ToLower(name[dot+1:])
	if !dashboardDateRE.MatchString(date) {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	if ext != "html" && ext != "pdf" {
		http.Error(w, "bad extension", http.StatusBadRequest)
		return
	}

	root := h.OIDashboardsDir
	filename := dashboardFilePrefix + date + "." + ext
	full := filepath.Join(root, filename)

	// Defense-in-depth: ensure the computed path is still inside the
	// configured root after filepath.Clean. The regex already prevents
	// "../" or alternate separators in the date, but this is cheap.
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		h.serverError(w, fmt.Errorf("abs root: %w", err))
		return
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		h.serverError(w, fmt.Errorf("abs file: %w", err))
		return
	}
	if !strings.HasPrefix(fullAbs+string(os.PathSeparator), rootAbs+string(os.PathSeparator)) &&
		fullAbs != rootAbs {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	if !fileExists(full) {
		http.NotFound(w, r)
		return
	}

	if ext == "pdf" {
		w.Header().Set("Content-Type", "application/pdf")
		disposition := "inline"
		if r.URL.Query().Get("download") == "1" {
			disposition = "attachment"
		}
		w.Header().Set("Content-Disposition", disposition+"; filename=\""+filename+"\"")
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	http.ServeFile(w, r, full)
}

// listDashboardDates walks the dashboards root and returns the dates
// (YYYY-MM-DD strings) for which an HTML dashboard exists, sorted
// reverse-chrono. The HTML is authoritative because that's what the
// /oi view loads in the iframe; a date with only a PDF would have no
// in-app rendering, so we'd surface it as missing rather than half-
// working.
//
// A missing root returns an empty slice (not an error) - the empty
// state in the template handles it.
func listDashboardDates(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, dashboardFilePrefix) || !strings.HasSuffix(name, ".html") {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, dashboardFilePrefix), ".html")
		if !dashboardDateRE.MatchString(date) {
			continue
		}
		out = append(out, date)
	}
	// Reverse-chrono: lexicographic descending works for YYYY-MM-DD.
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// fileURL returns the /oi/file/... URL for a given date+ext, or empty
// when the date is empty (no dashboard selected).
func fileURL(date, ext string) string {
	if date == "" {
		return ""
	}
	return "/oi/file/" + date + "." + ext
}

// fileExists is a tiny stat-only existence check that ignores the
// distinction between "not found" and "permission denied" because
// either makes the file effectively missing from our perspective.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
