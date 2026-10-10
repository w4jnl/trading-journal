package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// Every journal write bumps the user's live-data version: the HTML day
// save, the shared append-note helper (API, MCP, AI, GEX annotator) and the
// tag assignment helper. Before this, open pages only learned about group
// creates and GEX refreshes.
func TestJournalWritesBumpLiveData(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier()}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot"})
	v := h.LiveData.Version(uid)
	bumped := func(what string) {
		t.Helper()
		if nv := h.LiveData.Version(uid); nv == v {
			t.Fatalf("%s did not bump the live-data version", what)
		} else {
			v = nv
		}
	}

	form := url.Values{"body": {"first note"}}
	req := httptest.NewRequest(http.MethodPost, "/journal/2026-10-09", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("date", "2026-10-09")
	rec := httptest.NewRecorder()
	h.JournalSave(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("JournalSave -> %d: %s", rec.Code, rec.Body.String())
	}
	bumped("day save")

	entry, err := h.Queries.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: &uid, EntryDate: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.appendOrSeedJournalNote(ctx, entry, "appended"); err != nil {
		t.Fatal(err)
	}
	bumped("append note")

	if ok, err := h.assignTagIfUnset(ctx, entry.ID, "good"); err != nil || !ok {
		t.Fatalf("assign tag: ok=%v err=%v", ok, err)
	}
	bumped("assign tag")
	if ok, _ := h.assignTagIfUnset(ctx, entry.ID, "bad"); ok {
		t.Fatal("tag overwritten")
	}
	if nv := h.LiveData.Version(uid); nv != v {
		t.Fatal("a no-op tag assignment bumped the version")
	}
}
