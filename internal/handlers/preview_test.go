package handlers

import (
	"strings"
	"testing"
)

func TestPreviewTextStripsMachineBlocksAndMarkdown(t *testing.T) {
	body := "---\n\n_Gamma context (GEX) — futures prices_\n\n|  | entry 09:37 ET | exit 09:37 ET |\n|---|---|---|\n| regime | negative (P0) | negative (P0) |\n\n" +
		"<!-- tj-gex:v1 market=ES regime=negative stale=true -->\n\n" +
		"---\n\n_Note added 2026-09-28 17:00 CEST_\n\n### AI review — 2026-08-01\n\n" +
		"- **Execution:** Entered early in the session (07:33 ET) and kept a *short* bias.\n" +
		"- Risk: see ![chart](/journal/attachments/12) and [the plan](/library/strategies/3).\n\n<!-- tj-ai:v1 -->"
	got := previewText(body, 0)
	want := "Execution: Entered early in the session (07:33 ET) and kept a short bias. Risk: see and the plan."
	if got != want {
		t.Errorf("previewText =\n%q\nwant\n%q", got, want)
	}
	// Truncation counts runes and appends an ellipsis.
	if got := previewText(body, 20); got != "Execution: Entered e…" {
		t.Errorf("truncated = %q", got)
	}
	// An entry that is only machine blocks previews as empty.
	if got := previewText("_Gamma context (GEX) — futures prices_\n\n| a | b |\n\n<!-- tj-gex:v1 x=1 -->", 180); got != "" {
		t.Errorf("machine-only body previewed as %q", got)
	}
	// Plain text survives, including underscores inside identifiers.
	if got := previewText("Scalped MESU6 on APEX_100002 twice.", 180); got != "Scalped MESU6 on APEX_100002 twice." {
		t.Errorf("plain body = %q", got)
	}
	if !hasAIReview(body) || !hasGexContext(body) || hasAIReview("plain") || hasGexContext("plain") {
		t.Error("body flags wrong")
	}
	if strings.Contains(previewText(body, 0), "|") {
		t.Error("table pipes leaked into the preview")
	}
}
