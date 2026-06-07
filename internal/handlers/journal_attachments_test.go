package handlers

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestUserReferencesAttachment guards the API attachment-serve boundary: an
// entry_id=0 image is served over the API only when the CALLER's own strategy
// or journal body references it. Two failure modes are checked: cross-user
// access (must be denied) and the GLOB digit-boundary (id 1 must not match a
// reference to /attachments/12). The latter regressed once because SQLite GLOB
// negates a character class with "^", not "!" - "[!0-9]" silently matched
// nothing, failing every check closed.
func TestUserReferencesAttachment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ref.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE strategies (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, body TEXT NOT NULL)`,
		`CREATE TABLE journal_entries (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, body TEXT NOT NULL)`,
		// user 1: journal body references attachment 1, strategy references 5.
		`INSERT INTO journal_entries (id, user_id, body) VALUES (10, 1, 'see ![](/journal/attachments/1) and notes')`,
		`INSERT INTO strategies (id, user_id, body) VALUES (20, 1, 'setup ![](/journal/attachments/5)')`,
		// user 1: a body that references attachment 12 (NOT 1) - digit boundary.
		`INSERT INTO journal_entries (id, user_id, body) VALUES (11, 1, 'later ![](/journal/attachments/12)')`,
		// user 2: references attachment 7 - belongs to a different user.
		`INSERT INTO journal_entries (id, user_id, body) VALUES (30, 2, 'mine ![](/journal/attachments/7)')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}

	h := &Handlers{DB: db}
	ctx := context.Background()

	cases := []struct {
		name string
		uid  int64
		id   int64
		want bool
	}{
		{"own journal ref", 1, 1, true},
		{"own strategy ref", 1, 5, true},
		{"own ref to 12", 1, 12, true},
		{"digit boundary: 1 must not match /12", 1, 1, true}, // 1 is genuinely referenced (entry 10)
		{"unreferenced id", 1, 9, false},
		{"cross-user: user 1 cannot claim user 2's ref", 1, 7, false},
		{"user 2 owns ref 7", 2, 7, true},
		{"user 2 has no ref to 1", 2, 1, false},
	}
	for _, c := range cases {
		if got := h.userReferencesAttachment(ctx, c.uid, c.id); got != c.want {
			t.Errorf("%s: userReferencesAttachment(uid=%d, id=%d) = %v, want %v", c.name, c.uid, c.id, got, c.want)
		}
	}

	// Explicit digit-boundary: remove the genuine ref to 1, leave only /12,
	// and confirm id 1 no longer matches (the "^" vs "!" regression).
	if _, err := db.Exec(`DELETE FROM journal_entries WHERE id = 10`); err != nil {
		t.Fatal(err)
	}
	if h.userReferencesAttachment(ctx, 1, 1) {
		t.Error("id 1 matched a body that only references /attachments/12 - digit boundary broken")
	}
	if !h.userReferencesAttachment(ctx, 1, 12) {
		t.Error("id 12 should still be referenced after removing the id-1 ref")
	}
}
