package handlers

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/ast"
	mdHtml "github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"

	tj "github.com/w4jnl/trading-journal"
)

// The in-app API reference: API.md (embedded from the repo root via the
// tradingjournal root package) rendered through the same sanitized markdown
// pipeline the journal uses, plus a table of contents from its headings.
// API.md stays the single source of truth - this page never carries prose
// of its own.

// mdHeading is one table-of-contents entry (## and ### levels).
type mdHeading struct {
	Level int
	ID    string
	Text  string
}

// renderMarkdownDoc is renderMarkdown plus a heading walk for the TOC. Same
// per-call parser/renderer rule (see renderMarkdown's doc comment), same
// sanitizer - bluemonday's UGC policy keeps heading ids, tables and
// <pre><code>, so the TOC anchors resolve without any script.
func renderMarkdownDoc(src string) (string, []mdHeading) {
	p := parser.NewWithExtensions(parser.CommonExtensions | parser.AutoHeadingIDs)
	doc := p.Parse([]byte(src))
	var toc []mdHeading
	ast.WalkFunc(doc, func(n ast.Node, entering bool) ast.WalkStatus {
		h, ok := n.(*ast.Heading)
		if !ok || !entering || h.Level < 2 || h.Level > 3 {
			return ast.GoToNext
		}
		var b strings.Builder
		ast.WalkFunc(h, func(c ast.Node, entering bool) ast.WalkStatus {
			if entering {
				if leaf := c.AsLeaf(); leaf != nil {
					b.Write(leaf.Literal)
				}
			}
			return ast.GoToNext
		})
		toc = append(toc, mdHeading{Level: h.Level, ID: h.HeadingID, Text: b.String()})
		return ast.GoToNext
	})
	renderer := mdHtml.NewRenderer(mdHtml.RendererOptions{Flags: mdHtml.CommonFlags})
	unsafe := markdown.Render(doc, renderer)
	return string(mdSanitizer.SanitizeBytes(unsafe)), toc
}

// APIDocsPage: GET /settings/api-docs - API.md rendered in-app (session
// auth). The placeholder host in the examples is replaced by the host this
// request arrived on, so every curl line is copy-pasteable as-is.
func (h *Handlers) APIDocsPage(w http.ResponseWriter, r *http.Request) {
	md := string(tj.APIDoc)
	base := h.publicBaseURL(r)
	if base != "" {
		md = strings.ReplaceAll(md, "https://YOUR_HOST", base)
	} else {
		base = "https://YOUR_HOST"
	}
	doc, toc := renderMarkdownDoc(md)
	h.renderPage(w, r, "api_docs.html", map[string]any{
		"NavActive": "settings",
		"BackHref":  validBackHref(r),
		"Doc":       template.HTML(doc), // sanitized by mdSanitizer
		"TOC":       toc,
		"BaseURL":   base,
		"SpecHref":  "/settings/api-docs/openapi.json",
	})
}
