package aimethod

import (
	"os"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/journal"
)

// The rendered prompts equal the committed method text: a change to the
// method is a deliberate edit of a golden file and a version bump, never a
// side effect. (The v1 files were the handlers' constants before the
// package; v2 reads bars as one string per row and the day's lessons.)
func TestSystemPromptMatchesGolden(t *testing.T) {
	for _, tc := range []struct {
		scope Scope
		lang  string
		file  string
	}{
		{ScopeTrade, "", "system_trade_v2.txt"}, {ScopeGroup, "", "system_group_v2.txt"},
		{ScopeDay, "", "system_day_v2.txt"}, {ScopeWeek, "", "system_week_v2.txt"},
		{ScopeTrade, "Czech", "system_trade_czech_v2.txt"},
	} {
		want, err := os.ReadFile("testdata/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		if got := SystemPrompt(tc.scope, tc.lang); got != string(want) {
			t.Errorf("%s/%q differs from the golden file (%d vs %d bytes)", tc.scope, tc.lang, len(got), len(want))
		}
	}
	if SystemPrompt("position", "") != SystemPrompt(ScopeTrade, "") {
		t.Error("an unknown scope must read as a trade")
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
