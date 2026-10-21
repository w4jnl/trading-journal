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
	if Version != "2" {
		t.Error("the method version must move past the pre-package method")
	}
}
