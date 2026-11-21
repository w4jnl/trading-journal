package handlers

import (
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The engine's /health during the 2026-11-20 CBOE 403 outage: one "cycle
// failing" reason carrying four httpx exceptions with URLs, and a line per
// market. The banner gets one clause per fact with the markets grouped.
const engineHealth403 = `{"status": "degraded", "reasons": ["cycle failing: RuntimeError('cycle produced no market views: fetch failed: NDX: HTTPStatusError(\"Client error \\'403 Forbidden\\' for url \\'https://cdn-api.cboe.com/api/global/delayed_quotes/options/_NDX.json\\'\\\\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/403\"); fetch failed: QQQ: HTTPStatusError(\"Client error \\'403 Forbidden\\' for url \\'https://cdn-api.cboe.com/api/global/delayed_quotes/options/QQQ.json\\'\\\\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/403\"); fetch failed: SPX: HTTPStatusError(\"Client error \\'403 Forbidden\\' for url \\'https://cdn-api.cboe.com/api/global/delayed_quotes/options/_SPX.json\\'\\\\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/403\"); fetch failed: SPY: HTTPStatusError(\"Client error \\'403 Forbidden\\' for url \\'https://cdn-api.cboe.com/api/global/delayed_quotes/options/SPY.json\\'\\\\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/403\"); ES: skipped this cycle (missing leg); NQ: skipped this cycle (missing leg)')", "archive incomplete: NDX, QQQ, SPX, SPY", "ES: newest snapshot 62942s old", "NQ: newest snapshot 62950s old"], "notices": ["last cycle partial: NDX, QQQ, SPX, SPY", "reprice skipped: cached book is 62940s old", "ES: no marks", "NQ: no marks"]}`

func TestCondenseEngineNotes(t *testing.T) {
	cases := []struct{ in, want []string }{
		{
			in:   []string{"status degraded", "vendor host unreachable since 13:52Z", "no successful cycle this session", "marks stale"},
			want: []string{"status degraded", "vendor host unreachable since 13:52Z", "no successful cycle this session", "marks stale"},
		},
		{
			in:   []string{"ES: newest snapshot 62942s old", "NQ: newest snapshot 62950s old", "SPX: newest snapshot 400s old", "QQQ: newest snapshot 90s old"},
			want: []string{"newest snapshot 17h old: ES, NQ", "newest snapshot 7m old: SPX", "newest snapshot 90s old: QQQ"},
		},
		{
			in:   []string{"fetch failed: SPX: ReadTimeout('timed out')", "fetch failed: QQQ: ReadTimeout('timed out')"},
			want: []string{"fetch failed (timed out): SPX, QQQ"},
		},
	}
	for _, c := range cases {
		if got := condenseEngineNotes(c.in); strings.Join(got, "; ") != strings.Join(c.want, "; ") {
			t.Errorf("condense(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestGEXPageEngineNotesCondensed(t *testing.T) {
	db := gexTestDB(t)
	srv := gexQualityPageEngine(t, nil, engineHealth403)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test", Timeout: 2 * time.Second})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}

	body := gexPageGet(t, h, "/gex").Body.String()
	want := "Engine reports: status degraded; fetch failed (403 Forbidden): NDX, QQQ, SPX, SPY; skipped this cycle (missing leg): ES, NQ; archive incomplete: NDX, QQQ, SPX, SPY; newest snapshot 17h old: ES, NQ; last cycle partial: NDX, QQQ, SPX, SPY; reprice skipped: cached book is 17h old; no marks: ES, NQ."
	if !strings.Contains(body, want) {
		t.Errorf("banner missing %q in:\n%s", want, regexpFind(body, `(?s)<div class="flash info mb-16" role="status"[^>]*>.*?</div>`))
	}
	text := regexpFind(body, `Engine reports:[^<]*`)
	if strings.Contains(text, "mozilla") || strings.Contains(text, "HTTPStatusError") || strings.Contains(text, "https://") {
		t.Errorf("banner text still carries the raw exception: %q", text)
	}
	// The engine's own words stay on the banner's title for a hover.
	if !strings.Contains(body, `title="status degraded`) || !strings.Contains(body, "cdn-api.cboe.com") {
		t.Errorf("banner title missing the raw notes")
	}
}
