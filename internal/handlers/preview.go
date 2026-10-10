package handlers

import (
	"regexp"
	"strings"
)

// Preview text for journal bodies: the feed cards, the day page's recent
// entries, the editor's recall list and the trade sheet's group picker all
// show a one-line excerpt. The raw body is markdown with machine-generated
// blocks in it - the gamma-context table, its marker comment, the AI
// marker, note frames - so a naive prefix read as "--- _Gamma context (GEX)
// — futures prices_ | | entry 09:37 ET | ...". The excerpt is the first
// human-readable words after stripping those blocks and the markdown syntax.

var (
	// The gamma block: its italic heading through the closing marker comment.
	previewGexBlock = regexp.MustCompile(`(?s)_Gamma context \(GEX\)[^\n]*\n.*?<!--\s*` + regexp.QuoteMeta(gexMarker) + `[^>]*-->`)
	previewComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	previewNoteHead = regexp.MustCompile(`(?m)^_Note added [^\n]*_[ \t]*$`)
	previewAIHead   = regexp.MustCompile(`(?mi)^#{1,6}[ \t]*AI (review|daily synthesis|synthesis)[^\n]*$`)
	previewTable    = regexp.MustCompile(`(?m)^[ \t]*\|.*$`)
	previewRule     = regexp.MustCompile(`(?m)^[ \t]*(-{3,}|\*{3,}|_{3,})[ \t]*$`)
	previewHeading  = regexp.MustCompile(`(?m)^#{1,6}[ \t]+`)
	previewImage    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	previewLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	previewBullet   = regexp.MustCompile(`(?m)^[ \t]*(?:[-*+]|\d+[.)])[ \t]+`)
	previewQuote    = regexp.MustCompile(`(?m)^[ \t]*>[ \t]?`)
	previewEmphasis = regexp.MustCompile("\\*\\*|__|~~|`|\\*|(?:^|\\s)_|_(?:\\s|$)")
	previewSpace    = regexp.MustCompile(`\s+`)
)

// previewText returns the first n runes of the body's readable text, with
// an ellipsis when cut. "" when nothing human remains (an entry that is
// only a gamma block, say) - callers render their own empty state.
func previewText(body string, n int) string {
	s := previewGexBlock.ReplaceAllString(body, " ")
	s = previewComment.ReplaceAllString(s, " ")
	s = previewNoteHead.ReplaceAllString(s, " ")
	s = previewAIHead.ReplaceAllString(s, " ")
	s = previewTable.ReplaceAllString(s, " ")
	s = previewRule.ReplaceAllString(s, " ")
	s = previewHeading.ReplaceAllString(s, "")
	s = previewImage.ReplaceAllString(s, " ")
	s = previewLink.ReplaceAllString(s, "$1")
	s = previewBullet.ReplaceAllString(s, "")
	s = previewQuote.ReplaceAllString(s, "")
	s = previewEmphasis.ReplaceAllString(s, " ")
	s = strings.TrimSpace(previewSpace.ReplaceAllString(s, " "))
	r := []rune(s)
	if n > 0 && len(r) > n {
		return strings.TrimRight(string(r[:n]), " ") + "…"
	}
	return s
}

// hasAIReview / hasGexContext report the machine-generated blocks a body
// carries, so a card can badge them instead of previewing them.
func hasAIReview(body string) bool {
	return strings.Contains(body, "<!-- "+aiMarkerLocal) || strings.Contains(body, "<!-- "+aiMarkerMCP)
}

func hasGexContext(body string) bool { return strings.Contains(body, "<!-- "+gexMarker) }
