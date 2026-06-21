package handlers

import (
	"strings"
	"sync"
	"testing"
)

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
