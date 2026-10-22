package aimethod

import (
	"os"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/journal"
)

// The method block equals the committed text: a change to the method is a
// deliberate edit of the golden file and a version bump, never a side
// effect. (v1 was the handlers' constants, v2 read bars as strings, v3 is
// one block for every scope with the scope named in the user turn.)
func TestReviewMethodMatchesGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/review_method_v3.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := ReviewMethod(); got != string(want) {
		t.Errorf("ReviewMethod differs from the golden file (%d vs %d bytes)", len(got), len(want))
	}
}

// One block carries every scope's brief and both verdict lines; the scope
// line picks; the language wraps.
func TestReviewMethodShape(t *testing.T) {
	m := ReviewMethod()
	for _, s := range []string{"THIS ITEM IS ONE TRADE", "THIS ITEM IS A GROUP", "THIS ITEM IS A WHOLE TRADING DAY", "THIS ITEM IS A TRADING WEEK", "tj-ai:lesson key=", "mistakes=<keys|none>", "tj-ai:verdict tag=<good|neutral|bad|none> -->"} {
		if !strings.Contains(m, s) {
			t.Errorf("method lacks %q", s)
		}
	}
	if ScopeLine(ScopeDay) != "THIS ITEM IS A DAY: follow the day brief and end with the day/week verdict line (tag only)." || ScopeLine("position") != ScopeLine(ScopeTrade) {
		t.Errorf("scope line = %q", ScopeLine(ScopeDay))
	}
	p := SystemPrompt(ScopeWeek, "Czech")
	if !strings.HasPrefix(p, "WRITE YOUR ENTIRE REVIEW IN Czech") || !strings.HasSuffix(p, "not English.") || !strings.Contains(p, "THIS ITEM IS A WEEK") {
		t.Error("language wrapper or scope line missing")
	}
	if LanguageLead("") != "" || LanguageTail("") != "" {
		t.Error("the default language wraps nothing")
	}
}

// Every observable label is offered to the model, and the lesson line names
// every observable mistake key.
func TestVocabularyCoversLabels(t *testing.T) {
	v := LabelVocabulary()
	for _, kind := range []string{journal.KindMistake, journal.KindEmotion} {
		got := map[string]bool{}
		key := map[string]string{journal.KindMistake: "mistakes", journal.KindEmotion: "emotions"}[kind]
		for _, l := range v[key].([]map[string]string) {
			got[l["key"]] = true
		}
		for _, l := range journal.ObservableOptions(kind) {
			if !got[l.Key] {
				t.Errorf("%s %s missing from the vocabulary", kind, l.Key)
			}
		}
	}
	line := LessonLine()
	for _, k := range ObservableMistakeKeys() {
		if !strings.Contains(line, k) {
			t.Errorf("lesson line lacks %s", k)
		}
	}
	if Version == "1" || Version == "2" {
		t.Error("the method version must move past the pre-package method and the forced strategy pick")
	}
	// The strategy rule tests a fit, never forces one.
	for _, s := range []string{"Never pick a strategy by elimination", "none is the right answer for a trade that fits no playbook", "dormant, automated or an overlay"} {
		if !strings.Contains(VerdictDecision, s) {
			t.Errorf("verdict rule lacks %q", s)
		}
	}
	if strings.Contains(VerdictDecision, "not fully certain") || strings.Contains(VerdictDecision, "plausibly related") {
		t.Error("the verdict rule still forces a strategy pick")
	}
}

// The document, the instructions and the prompts carry the marker, the
// modes and the lines, and a prompt fills its arguments.
func TestMethodForAgents(t *testing.T) {
	doc := Document(DocOptions{Tools: []string{"prepare_day", "append_note"}, Language: "Czech"})
	for _, s := range []string{"# The review method, version " + Version, MarkerLine(), "## Modes", "`review_day`", "`today`", "## Language", "Czech", "`prepare_day`", "`append_note`", "tj-ai:lesson key=", "## The evidence", "`dayMap`"} {
		if !strings.Contains(doc, s) {
			t.Errorf("document lacks %q", s)
		}
	}
	ins := Instructions()
	if !strings.Contains(ins, MarkerLine()) || !strings.Contains(ins, "trading-journal://method") || !strings.Contains(ins, "server_info") || !strings.Contains(ins, "stale") {
		t.Errorf("instructions = %.200q", ins)
	}
	m, ok := ModeByName("review_entry")
	if !ok {
		t.Fatal("review_entry mode missing")
	}
	text := PromptText(m, map[string]string{"entryId": "42"}, "Czech", "THE TRADER: a scalper.")
	if !strings.Contains(text, "THE TRADER: a scalper.") {
		t.Error("the prompt lacks the profile")
	}
	if !strings.Contains(text, "entryId = 42") || !strings.Contains(text, "get_journal_entry") || !strings.Contains(text, "Czech") || !strings.HasSuffix(text, MarkerLine()+".") {
		t.Errorf("prompt text = %q", text)
	}
	if !strings.Contains(PrepareNote(), MarkerLine()) || MarkerLine() != "<!-- tj-ai-mcp:v1 method="+Version+" -->" {
		t.Errorf("prepare note / marker line = %q / %q", PrepareNote(), MarkerLine())
	}
	if len(Modes) != 5 {
		t.Errorf("modes = %d", len(Modes))
	}
}

// The profile parses with defaults, refuses unknown keys, renders a block
// only when it says something, and sizes the evidence by style.
func TestProfile(t *testing.T) {
	if p := ParseProfile(""); !p.IsDefault() || p.Style != DefaultStyle || p.Block() != "" || p.Encode() != "" {
		t.Errorf("default profile = %+v", p)
	}
	p := ParseProfile(`{"style":"scalper","focus":["gamma","levels","gamma"],"markets":["mes","ES"],"houseRules":"  Never add to a loser.  "}`)
	if p.Style != "scalper" || strings.Join(p.Focus, ",") != "gamma,levels" || strings.Join(p.Markets, ",") != "ES,MES" || p.HouseRules != "Never add to a loser." {
		t.Errorf("parsed = %+v", p)
	}
	blk := p.Block()
	for _, s := range []string{"a scalper", "gamma map", "short-term levels", "ES, MES", "STANDING INSTRUCTIONS", "Never add to a loser."} {
		if !strings.Contains(blk, s) {
			t.Errorf("block lacks %q", s)
		}
	}
	if e := p.Plan(); e.Bars1m != 45 || e.GammaBrief {
		t.Errorf("scalper plan = %+v", e)
	}
	if e := ParseProfile(`{"style":"swing","focus":["coach"]}`).Plan(); e.Bars1m != 0 || e.Bars15m != 78 || !e.GammaBrief {
		t.Errorf("swing plan = %+v", e)
	}
	bad := Profile{Style: "robot"}
	if err := bad.Validate(); err == nil {
		t.Error("unknown style accepted")
	}
	bad = Profile{Focus: []string{"astrology"}}
	if err := bad.Validate(); err == nil {
		t.Error("unknown focus accepted")
	}
	bad = Profile{HouseRules: strings.Repeat("x", houseRulesMax+1)}
	if err := bad.Validate(); err == nil {
		t.Error("overlong rules accepted")
	}
	round := ParseProfile(p.Encode())
	if round.Style != p.Style || strings.Join(round.Focus, ",") != strings.Join(p.Focus, ",") || round.HouseRules != p.HouseRules {
		t.Errorf("round trip = %+v", round)
	}
}
