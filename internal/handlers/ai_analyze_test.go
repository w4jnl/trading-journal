package handlers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/models"
)

func TestParseAIVerdict(t *testing.T) {
	cases := []struct {
		in    string
		tag   string
		strat string
	}{
		{"analysis…\n\n<!-- tj-ai:verdict tag=good strategy=12 -->", "good", "12"},
		{"<!-- tj-ai:verdict tag=bad strategy=none -->", "bad", "none"},
		{"<!-- tj-ai:verdict strategy=5 tag=neutral -->", "neutral", "5"},             // order-independent
		{`<!-- tj-ai:verdict tag=GOOD strategy="7" -->`, "good", "7"},                 // case + quotes
		{"<!-- tj-ai:verdict tag=good strategy=ORB VWAP -->", "good", "ORB VWAP"},     // NAME with a space
		{"<!-- tj-ai:verdict strategy=VWAP Bounce tag=bad -->", "bad", "VWAP Bounce"}, // name first
		{"no verdict line here", "", ""},
		{"<!-- tj-ai:verdict tag=weird strategy=abc -->", "", "abc"},                                          // bad tag dropped; raw strategy kept
		{"<!-- tj-ai:verdict tag=bad strategy=ORB VWAP mistakes=chased emotions=none -->", "bad", "ORB VWAP"}, // name stops at the next field
	}
	for _, c := range cases {
		v := parseAIVerdict(c.in)
		if v.Tag != c.tag || v.Strategy != c.strat {
			t.Errorf("parseAIVerdict(%q) = {tag:%q strategy:%q}, want {tag:%q strategy:%q}", c.in, v.Tag, v.Strategy, c.tag, c.strat)
		}
	}
}

// The label fields take comma- or space-separated keys in any order; "none"
// and empty mean nothing.
func TestParseAIVerdictLabels(t *testing.T) {
	cases := []struct {
		in                 string
		mistakes, emotions []string
	}{
		{"<!-- tj-ai:verdict tag=bad strategy=none mistakes=chased,late-exit emotions=rushed -->", []string{"chased", "late-exit"}, []string{"rushed"}},
		{"<!-- tj-ai:verdict emotions=Tilted, frustrated mistakes=revenge tag=bad strategy=3 -->", []string{"revenge"}, []string{"tilted", "frustrated"}},
		{"<!-- tj-ai:verdict tag=good strategy=1 mistakes=none emotions=none -->", nil, nil},
		{`<!-- tj-ai:verdict tag=good strategy=1 mistakes="chased" -->`, []string{"chased"}, nil},
		{"<!-- tj-ai:verdict tag=good strategy=1 -->", nil, nil},
	}
	for _, c := range cases {
		v := parseAIVerdict(c.in)
		if !reflect.DeepEqual(v.Mistakes, c.mistakes) || !reflect.DeepEqual(v.Emotions, c.emotions) {
			t.Errorf("parseAIVerdict(%q) = mistakes %q emotions %q, want %q / %q", c.in, v.Mistakes, v.Emotions, c.mistakes, c.emotions)
		}
	}
}

// Each scope gets its own brief and verdict line; an unknown scope reads as a
// trade; a language wraps the prompt but leaves the verdict keywords alone.
func TestAISystemPromptPerScope(t *testing.T) {
	for _, c := range []struct {
		scope, has string
		labels     bool
	}{
		{"trade", "ONE TRADE", true},
		{"group", "Copy consistency", true},
		{"day", "WHOLE TRADING DAY", false},
		{"week", "weeklyDigest", false},
		{"", "ONE TRADE", true},
	} {
		p := aiSystemPrompt(c.scope, "")
		if !strings.Contains(p, c.has) || !strings.Contains(p, "tj-ai:verdict tag=") {
			t.Errorf("%q prompt lacks %q or the verdict line", c.scope, c.has)
		}
		if got := strings.Contains(p, "mistakes=<keys|none>"); got != c.labels {
			t.Errorf("%q prompt asks for labels = %v, want %v", c.scope, got, c.labels)
		}
	}
	if p := aiSystemPrompt("day", "Czech"); !strings.HasPrefix(p, "WRITE YOUR ENTIRE REVIEW IN Czech") || !strings.Contains(p, "WHOLE TRADING DAY") {
		t.Errorf("language wrapper lost the scope part")
	}
}

func TestAIFilledLineAndEscape(t *testing.T) {
	en := aiWordsFor("")
	if got := aiSuggestedLine(aiAnalyzeResult{}, "", en); got != "" {
		t.Errorf("nothing filled = %q", got)
	}
	res := aiAnalyzeResult{Tag: "bad", StrategyID: 5, Mistakes: []string{"chased", "late-exit"}, Emotions: []string{"rushed"}}
	got := aiSuggestedLine(res, "ORB_v2", en)
	want := "*Suggested by this review: tag **bad** · strategy **ORB\\_v2** · mistakes **Chased the entry, Exited too late** · emotions **Rushed**.*"
	if got != want {
		t.Errorf("suggested line =\n%s\nwant\n%s", got, want)
	}
	// In the review language: the framing translated, the pill names kept.
	cs := aiSuggestedLine(res, "ORB", aiWordsFor("Czech"))
	if cs != "*Navrženo tímto hodnocením: štítek **bad** · strategie **ORB** · chyby **Chased the entry, Exited too late** · emoce **Rushed**.*" {
		t.Errorf("czech suggested line = %s", cs)
	}
	if got := mdEscapeInline("mlx-community/Qwen3_VL*4bit [x]"); got != "mlx-community/Qwen3\\_VL\\*4bit \\[x\\]" {
		t.Errorf("escape = %q", got)
	}
}

func TestResolveStrategyID(t *testing.T) {
	strats := []models.Strategy{{ID: 1, Name: "ORB VWAP"}, {ID: 2, Name: "VWAP Bounce"}}
	cases := []struct {
		raw  string
		want int64
	}{
		{"1", 1},
		{"2", 2},
		{"ORB VWAP", 1},    // exact name
		{"vwap bounce", 2}, // case-insensitive name
		{"none", 0},
		{"", 0},
		{"99", 0},               // numeric but not owned
		{"Unknown Strategy", 0}, // no name match
	}
	for _, c := range cases {
		if got := resolveStrategyID(c.raw, strats); got != c.want {
			t.Errorf("resolveStrategyID(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestStripAIVerdict(t *testing.T) {
	if got := stripAIVerdict("Analysis line.\n\n<!-- tj-ai:verdict tag=good strategy=1 -->"); got != "Analysis line." {
		t.Errorf("stripAIVerdict = %q, want %q", got, "Analysis line.")
	}
	if got := stripAIVerdict("no verdict"); got != "no verdict" {
		t.Errorf("stripAIVerdict left text unchanged = %q", got)
	}
}

// Every language offered in Settings frames its reviews in that language.
func TestAIWordsCoverEveryLanguage(t *testing.T) {
	for _, o := range aiLanguageOptions {
		w, ok := aiWordsByLanguage[o.Value]
		if !ok {
			t.Errorf("no review words for %q", o.Value)
			continue
		}
		for name, v := range map[string]string{"Review": w.Review, "NoteAdded": w.NoteAdded, "Via": w.Via, "ThisServer": w.ThisServer,
			"Suggested": w.Suggested, "Tag": w.Tag, "Strategy": w.Strategy, "Mistakes": w.Mistakes, "Emotions": w.Emotions} {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%q: %s is empty", o.Value, name)
			}
		}
	}
	if aiWordsFor("Klingon") != aiWordsFor("") {
		t.Error("an unknown language should fall back to English")
	}
	for _, c := range []struct {
		ref  aiModelRef
		lang string
		want string
	}{
		{aiModelRef{Model: "claude-sonnet-5-5", Provider: "Anthropic (Claude)"}, "", "claude-sonnet-5-5 via Anthropic (Claude)"},
		{aiModelRef{Model: "claude-sonnet-5-5", Provider: "Anthropic (Claude)"}, "Czech", "claude-sonnet-5-5 přes Anthropic (Claude)"},
		{aiModelRef{Model: "qwen3"}, "German", "qwen3 über diesen Server"},
		{aiModelRef{}, "", "this server"},
	} {
		if got := c.ref.Label(aiWordsFor(c.lang)); got != c.want {
			t.Errorf("Label(%+v, %q) = %q, want %q", c.ref, c.lang, got, c.want)
		}
	}
	if got := customHost("http://127.0.0.1:18790/v1"); got != "127.0.0.1:18790" {
		t.Errorf("customHost = %q", got)
	}
}

// A review reads the trader's own words: the blocks earlier reviews and the
// gamma annotator appended are dropped and counted, the rest is cut to
// the cap.
func TestTraderBody(t *testing.T) {
	body := "Took the 3bar at the open.\n\n---\n\n### AI review (x) — 2026-10-09\n\n- **Setup:** fine\n\n<!-- " + aiMarkerLocal + " method=2 -->\n---\n\n### GEX\n\n<!-- " + gexMarker + " -->\n---\n\nAdded: moved the stop too early.\n---\n\n### AI review (mcp)\n\n<!-- " + aiMarkerMCP + " -->"
	words, prior := traderBody(body, 4000)
	if prior != 2 || strings.Contains(words, "AI review") || strings.Contains(words, "GEX") || !strings.Contains(words, "Took the 3bar") || !strings.Contains(words, "moved the stop") {
		t.Errorf("traderBody = %q prior %d", words, prior)
	}
	if w, _ := traderBody(strings.Repeat("x", 50), 10); len([]rune(w)) != 12 || !strings.HasSuffix(w, " …") {
		t.Errorf("cap = %q", w)
	}
	if w, n := traderBody("", 100); w != "" || n != 0 {
		t.Errorf("empty = %q %d", w, n)
	}
}
