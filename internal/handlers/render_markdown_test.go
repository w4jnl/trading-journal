package handlers

import (
	"strings"
	"sync"
	"testing"
)

// TestRenderMarkdownSanitizes verifies that stored-XSS vectors in a journal /
// strategy body are stripped before the HTML reaches template.HTML. This matters
// because the app is multi-user with admin act-as: a payload one user stores can
// otherwise execute in an admin's session when the admin views their journal.
func TestRenderMarkdownSanitizes(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		absent  []string // substrings that must NOT survive
		present []string // benign content that must survive
	}{
		{
			name:   "script tag",
			src:    "hello\n\n<script>alert(document.cookie)</script>\n",
			absent: []string{"<script", "alert(document.cookie)"},
			// paragraph text still renders
			present: []string{"hello"},
		},
		{
			name:   "img onerror handler",
			src:    "<img src=x onerror=\"fetch('/admin/users',{method:'POST'})\">",
			absent: []string{"onerror", "fetch('/admin/users'"},
		},
		{
			name:   "javascript link scheme",
			src:    "[click me](javascript:alert(1))",
			absent: []string{"javascript:"},
			// the link text is preserved even though the href is dropped
			present: []string{"click me"},
		},
		{
			name:    "benign markdown untouched",
			src:     "# Title\n\n**bold** and a [link](https://example.com) and `code`.\n\n![shot](/journal/attachments/12)",
			present: []string{"<strong>bold</strong>", "<code>code</code>", "https://example.com", "/journal/attachments/12"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := renderMarkdown(c.src)
			for _, bad := range c.absent {
				if strings.Contains(out, bad) {
					t.Errorf("sanitized output still contains %q\n---\n%s", bad, out)
				}
			}
			for _, good := range c.present {
				if !strings.Contains(out, good) {
					t.Errorf("expected %q to survive sanitization\n---\n%s", good, out)
				}
			}
		})
	}
}

// TestRenderMarkdownConcurrent guards against the shared-renderer regression: a
// package-level *html.Renderer is NOT goroutine-safe with AutoHeadingIDs (it keeps a
// heading-ID dedup map), so concurrent requests crashed the process with
// "fatal error: concurrent map writes". The duplicate headings below force the dedup
// path (EnsureUniqueHeadingID). Run with -race; must stay clean.
func TestRenderMarkdownConcurrent(t *testing.T) {
	src := "## AI review (Opus 4.8)\n\nfirst\n\n### Execution\n\nx\n\n## AI review (Opus 4.8)\n\nsecond\n\n### Execution\n\ny"
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				if !strings.Contains(renderMarkdown(src), "AI review") {
					t.Errorf("rendered output missing heading text")
					return
				}
			}
		}()
	}
	wg.Wait()
}
