package aimethod

// SystemPrompt renders the in-app system prompt of a review: the generic
// skill, the scope's skill, the lesson line and the verdict line. An
// unknown scope reads as a trade. language, when set ("Czech"), wraps the
// prompt so the prose comes out in that language while the machine-readable
// lines stay English: local models default to English when the directive
// is buried at the end of a long English prompt, so it leads and closes.
func SystemPrompt(scope Scope, language string) string {
	skill, ok := Scopes[scope]
	if !ok {
		skill = Scopes[ScopeTrade]
	}
	verdict := VerdictDecision
	if scope == ScopeDay || scope == ScopeWeek {
		verdict = VerdictTagOnly
	}
	body := Role + "\n\n" + Grounding + "\n\n" + DayMapRule + "\n\n" + Format + "\n\n" + skill.Brief + "\n\n" + LessonLine() + "\n\n" + verdict
	if language == "" {
		return body
	}
	lead := "WRITE YOUR ENTIRE REVIEW IN " + language + ". Every sentence, every bold section label, and every bullet must be in " + language + " - even though these instructions are written in English. The section labels in the brief below (Setup & entry, Risk, Exit, ...) are English placeholders: write each of them in " + language + " too. The ONLY parts that stay in English are the two machine-readable lines' keywords and label keys (the literal <!-- tj-ai:lesson ... --> and <!-- tj-ai:verdict ... --> comments); the lesson sentence inside the lesson line is written in " + language + ".\n\n"
	tail := "\n\nReminder: all prose, the section labels and the lesson sentence must be written in " + language + ", not English."
	return lead + body + tail
}
