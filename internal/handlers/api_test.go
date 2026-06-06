package handlers

import (
	"strings"
	"testing"
	"time"
)

func TestApiRewriteAttachmentRefs(t *testing.T) {
	md := "a ![](/journal/attachments/88) b ![](/journal/attachments/5) c ![](/journal/attachments/88)"
	out, ids := apiRewriteAttachmentRefs(md)
	if strings.Contains(out, "/journal/attachments/") {
		t.Errorf("refs not rewritten: %s", out)
	}
	if !strings.Contains(out, "/api/v1/attachments/88") || !strings.Contains(out, "/api/v1/attachments/5") {
		t.Errorf("output missing api refs: %s", out)
	}
	if len(ids) != 2 || ids[0] != 88 || ids[1] != 5 {
		t.Errorf("ids = %v, want [88 5] (deduped, first-seen order)", ids)
	}

	out2, ids2 := apiRewriteAttachmentRefs("no images here")
	if out2 != "no images here" || len(ids2) != 0 {
		t.Errorf("no-ref input changed: %q ids=%v", out2, ids2)
	}
}

func TestFormatAppendedNote(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 6, 6, 14, 32, 0, 0, time.UTC) // 15:32 CET
	got := formatAppendedNote(now, loc, "did X")
	want := "\n\n---\n\n_Note added 2026-06-06 15:32 CET_\n\ndid X\n"
	if got != want {
		t.Errorf("formatAppendedNote =\n%q\nwant\n%q", got, want)
	}
	// Trailing whitespace on the note is trimmed; the block always ends in \n.
	if !strings.HasSuffix(formatAppendedNote(now, loc, "hi\n\n  "), "hi\n") {
		t.Error("trailing whitespace not trimmed to single newline")
	}
}
