package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The sheet opens on the trade: no note shows one "Add note" line with the
// editor folded; an existing note shows its preview; ?journal=1 (the row's
// journal icon) unfolds the editor in both cases. The row carries the icon
// as a button the sheet script handles.
func TestTradeSheetJournalFolded(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + itoa64(uid) + `, 'mine', 'motivewave_csv', 'A-1')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
			(10, 1, 'ESZ6', 'ES', 'long', 1, 7250, 7252, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:05:00 +0000 UTC', 'closed', 96),
			(11, 1, 'ESZ6', 'ES', 'long', 1, 7250, 7252, '2026-10-09 15:00:00 +0000 UTC', '2026-10-09 15:05:00 +0000 UTC', 'closed', 96)`,
		`INSERT INTO journal_entries (id, user_id, trade_id, entry_date, body, scope) VALUES (1, ` + itoa64(uid) + `, 11, '2026-10-09', 'held through the pullback', 'trade')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	sheet := func(id, qs string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/trades/"+id+qs, nil).WithContext(ctx)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.TradeDetail(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("sheet %s%s -> %d", id, qs, rec.Code)
		}
		return rec.Body.String()
	}
	has := func(body string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("missing %q", w)
			}
		}
	}

	// No note, row click: folded.
	b := sheet("10", "")
	has(b, `class="journal-view journal-view--empty" data-journal-view>`, `data-journal-edit title="Add a note (e)">+ Add note<`, `class="journal-form" hidden>`)
	if strings.Contains(b, "data-journal-open") {
		t.Error("row click should not unfold the editor")
	}
	// No note, journal icon: editor unfolded and flagged for focus, the empty line hidden.
	b = sheet("10", "?journal=1")
	has(b, `class="journal-form" data-journal-open>`, `journal-view--empty" data-journal-view hidden>`)
	// Existing note, row click: preview shown, editor folded.
	b = sheet("11", "")
	has(b, `<div class="journal-view" data-journal-view>`, `held through the pullback`, `class="journal-form" hidden>`)
	// Existing note, journal icon: preview hidden, editor open with the body.
	b = sheet("11", "?journal=1")
	has(b, `<div hidden data-journal-view>`, `class="journal-form" data-journal-open>`, `>held through the pullback</textarea>`)

	// The row: the icon is a button the sheet script handles, inside the
	// span the save response swaps out of band.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /trades", h.TradesPage)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/trades", nil).WithContext(ctx))
	has(rec.Body.String(),
		`<button type="button" class="j-open" data-journal-open="10" title="Add a note" aria-label="Add a note to trade ESZ6"><span id="trow-jicon-10">`,
		`data-journal-open="11" title="Edit the note"`,
		`<kbd>e</kbd> note`)
}
