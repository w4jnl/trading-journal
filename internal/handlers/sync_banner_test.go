package handlers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// TestSyncBanner covers the three render states: keep polling while nothing
// new has arrived, show the "N new fills" banner once a live sync ingests
// past the baseline, and go inert after the poll window with nothing.
func TestSyncBanner(t *testing.T) {
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")
	h := &Handlers{Queries: q, Templates: web.TemplatesFS}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})

	call := func(query string) string {
		req := httptest.NewRequest("GET", "/sync/banner"+query, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.SyncBanner(rec, req)
		return rec.Body.String()
	}

	// First call (no base): captures baseline 0 (empty log), nothing new,
	// still within the poll window -> a polling div carrying base=0.
	body := call("")
	if !strings.Contains(body, `hx-trigger="every 3s"`) || !strings.Contains(body, "base=0") {
		t.Fatalf("first call should poll with base=0, got: %s", body)
	}

	// A live sync ingests 3 fills (sync_log row).
	msg := "gateway holding, 3 new fills"
	uidPtr := uid
	if _, err := q.CreateSyncLog(ctx, models.CreateSyncLogParams{
		UserID: &uidPtr, SyncType: "ibkr_api", Status: "success", ExecsAdded: 3, Message: &msg,
	}); err != nil {
		t.Fatalf("CreateSyncLog: %v", err)
	}

	// Poll again with base=0 -> banner appears with the fill count.
	body = call("?base=0&n=1")
	if !strings.Contains(body, "3 new fills") || !strings.Contains(body, "Refresh") {
		t.Errorf("expected the new-fills banner, got: %s", body)
	}
	if strings.Contains(body, "hx-trigger") {
		t.Errorf("banner state must stop polling, got: %s", body)
	}

	// A page loading AFTER the sync captures a higher baseline; with nothing
	// newer and the poll window elapsed, the banner goes inert.
	base, _ := q.MaxSyncLogIDForUser(ctx, &uidPtr)
	body = call("?base=" + itoa64(base) + "&n=99")
	if strings.Contains(body, "hx-trigger") || strings.Contains(body, "new fill") {
		t.Errorf("expected an inert div, got: %s", body)
	}
}
