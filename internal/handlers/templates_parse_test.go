package handlers

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/web"
)

// TestPageTemplatesParse parses every page template (base + page + all partials,
// exactly as parsedPage does at render time) so a template syntax error or an
// unknown funcMap function is caught in CI instead of on first page view.
func TestPageTemplatesParse(t *testing.T) {
	h := &Handlers{Templates: web.TemplatesFS}

	entries, err := fs.Glob(web.TemplatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	var pages int
	for _, e := range entries {
		name := strings.TrimPrefix(e, "templates/")
		if strings.HasPrefix(name, "_") || name == "base.html" {
			continue // partials and the base layout are parsed as siblings
		}
		pages++
		if _, err := h.parsedPage(name); err != nil {
			t.Errorf("parse %s: %v", name, err)
		}
	}
	if pages == 0 {
		t.Fatal("no page templates found - glob or embed path changed")
	}
}
