package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The docs renderer keeps what a TOC and API.md need through the sanitizer:
// heading ids, tables, code blocks and in-page anchors.
func TestRenderMarkdownDoc(t *testing.T) {
	src := "# Title\n\n## Authentication\n\ntext\n\n### Presenting a key\n\n" +
		"```jsonc\n{ \"a\": 1 } // c\n```\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n" +
		"See [auth](#authentication).\n"
	html, toc := renderMarkdownDoc(src)
	if len(toc) != 2 || toc[0] != (mdHeading{2, "authentication", "Authentication"}) ||
		toc[1] != (mdHeading{3, "presenting-a-key", "Presenting a key"}) {
		t.Errorf("toc = %+v", toc)
	}
	for _, want := range []string{`<h2 id="authentication">`, `<h3 id="presenting-a-key">`, "<pre><code>", "<table>", `href="#authentication"`} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "<h1") && len(toc) != 2 {
		t.Errorf("h1 must not enter the TOC")
	}
}

// The in-app page renders the embedded API.md with the request host
// substituted for the placeholder, so the examples are copy-pasteable.
func TestAPIDocsPageRenders(t *testing.T) {
	conn, q := newAuthzTestDB(t)
	u, err := q.CreateUser(context.Background(), models.CreateUserParams{Username: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC}
	req := httptest.NewRequest("GET", "/settings/api-docs", nil)
	req.Host = "journal.example:8080"
	// DisplayName matters: base.html renders the sidebar avatar from its first letter.
	req = req.WithContext(auth.WithUser(req.Context(), &auth.User{ID: u.ID, Username: u.Username, DisplayName: "Docs"}))
	rec := httptest.NewRecorder()
	h.APIDocsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="daily-stats"`, `href="#daily-stats"`, // TOC anchor + heading from API.md
		`id="authentication"`,
		"http://journal.example:8080/api/v1", // placeholder host substituted
		"<table",
		`href="/settings/api-docs/openapi.json"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(body, "YOUR_HOST") {
		t.Errorf("placeholder host left unsubstituted")
	}
}
