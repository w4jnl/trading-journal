package aimethod

import (
	"sort"
	"strings"
)

// ReviewMethod is the whole review method as one block, the same for every
// scope: the generic skill, then each scope's skill under a heading, the
// lesson line and both verdict lines. One block means one cached prefix
// across a day run's trade, group and day reviews; the user turn says
// which item this is (ScopeLine).
func ReviewMethod() string {
	var b strings.Builder
	b.WriteString(Role)
	b.WriteString("\n\nThe user message names the item's kind on its first line (THIS ITEM IS ...): follow that kind's brief below and its verdict line, and ignore the other briefs. The context arrives as a few consecutive JSON objects: the label vocabulary and the playbooks first (they rarely change), then the item.\n\n")
	b.WriteString(Grounding)
	b.WriteString("\n\n")
	b.WriteString(DayMapRule)
	b.WriteString("\n\n")
	b.WriteString(Format)
	keys := make([]string, 0, len(Scopes))
	for k := range Scopes {
		keys = append(keys, string(k))
	}
	sort.Slice(keys, func(i, j int) bool { return scopeOrder[Scope(keys[i])] < scopeOrder[Scope(keys[j])] })
	for _, k := range keys {
		b.WriteString("\n\n==== WHEN " + Scopes[Scope(k)].Brief)
	}
	b.WriteString("\n\n==== EVERY ITEM\n\n")
	b.WriteString(LessonLine())
	b.WriteString("\n\nFor a trade or a group: ")
	b.WriteString(VerdictDecision)
	b.WriteString("\n\nFor a day or a week: ")
	b.WriteString(VerdictTagOnly)
	return b.String()
}

var scopeOrder = map[Scope]int{ScopeTrade: 0, ScopeGroup: 1, ScopeDay: 2, ScopeWeek: 3}

// ScopeLine opens the user turn: which brief applies. An unknown scope
// reads as a trade.
func ScopeLine(scope Scope) string {
	if _, ok := Scopes[scope]; !ok {
		scope = ScopeTrade
	}
	verdict := "the trade/group verdict line"
	if scope == ScopeDay || scope == ScopeWeek {
		verdict = "the day/week verdict line (tag only)"
	}
	return "THIS ITEM IS A " + strings.ToUpper(string(scope)) + ": follow the " + string(scope) + " brief and end with " + verdict + "."
}

// LanguageLead and LanguageTail wrap a prompt for a review language: local
// models default to English when the directive is buried at the end of a
// long English prompt, so it leads and closes. Empty for the default.
func LanguageLead(language string) string {
	if language == "" {
		return ""
	}
	return "WRITE YOUR ENTIRE REVIEW IN " + language + ". Every sentence, every bold section label, and every bullet must be in " + language + " - even though these instructions are written in English. The section labels in the brief below (Setup & entry, Risk, Exit, ...) are English placeholders: write each of them in " + language + " too. The ONLY parts that stay in English are the two machine-readable lines' keywords and label keys (the literal <!-- tj-ai:lesson ... --> and <!-- tj-ai:verdict ... --> comments); the lesson sentence inside the lesson line is written in " + language + "."
}

func LanguageTail(language string) string {
	if language == "" {
		return ""
	}
	return "Reminder: all prose, the section labels and the lesson sentence must be written in " + language + ", not English."
}

// SystemPrompt is the method and the scope line as one string, for a client
// that takes a flat prompt and for tests.
func SystemPrompt(scope Scope, language string) string {
	body := ReviewMethod() + "\n\n" + ScopeLine(scope)
	if language == "" {
		return body
	}
	return LanguageLead(language) + "\n\n" + body + "\n\n" + LanguageTail(language)
}
