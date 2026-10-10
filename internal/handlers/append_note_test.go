package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// TestAppendOrSeedJournalNote covers the placeholder behavior: the first note on a
// still-pending entry REPLACES the prepare_day placeholder (keeping the leading
// divider, dropping the "pending" line), while every later note - and any note on an
// entry that already holds real content - is appended without clobbering.
func TestAppendOrSeedJournalNote(t *testing.T) {
	db := dbtest.Open(t, "notes")
	if _, err := db.Exec(`INSERT INTO users (id, username) VALUES (7, 'seven')`); err != nil {
		t.Fatal(err)
	}
	// id 1: a freshly prepared, not-yet-analyzed entry (the placeholder).
	// id 2: an entry that already holds real content.
	day := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (1, 7, ?, ?, 'day')`, aiPlaceholderBody, day); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (2, 7, 'pre-existing content', ?, 'day')`, day); err != nil {
		t.Fatal(err)
	}

	h := &Handlers{DB: db, Queries: models.New(db), Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7})

	body := func(id int64) string {
		var b string
		if err := db.QueryRow(`SELECT body FROM journal_entries WHERE id = ?`, id).Scan(&b); err != nil {
			t.Fatalf("read body %d: %v", id, err)
		}
		return b
	}

	review := "### AI review — 2026-05-12\n\nClean entry, good size.\n\n<!-- tj-ai:v1 -->"

	// 1) First note on the pending entry: seed (replace placeholder).
	if err := h.appendOrSeedJournalNote(ctx, models.JournalEntry{ID: 1, Body: aiPlaceholderBody}, review); err != nil {
		t.Fatal(err)
	}
	got := body(1)
	if strings.Contains(got, "pending") {
		t.Errorf("placeholder not removed on seed: %q", got)
	}
	want := "---\n\n" + review + "\n"
	if got != want {
		t.Errorf("seed body =\n%q\nwant\n%q", got, want)
	}

	// 2) Second note on the same (now real) entry: append, never clobber.
	if err := h.appendOrSeedJournalNote(ctx, models.JournalEntry{ID: 1, Body: got}, "Follow-up thought."); err != nil {
		t.Fatal(err)
	}
	got2 := body(1)
	if !strings.HasPrefix(got2, want) {
		t.Errorf("second append clobbered the review: %q", got2)
	}
	if !strings.Contains(got2, "_Note added ") || !strings.Contains(got2, "Follow-up thought.") {
		t.Errorf("second note not appended as a dated block: %q", got2)
	}

	// 3) A note on an entry that already has real content always appends.
	if err := h.appendOrSeedJournalNote(ctx, models.JournalEntry{ID: 2, Body: "pre-existing content"}, "added later"); err != nil {
		t.Fatal(err)
	}
	got3 := body(2)
	if !strings.HasPrefix(got3, "pre-existing content") {
		t.Errorf("clobbered pre-existing content: %q", got3)
	}
	if !strings.Contains(got3, "added later") {
		t.Errorf("note not appended: %q", got3)
	}
}
