package handlers

import (
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

// The reconcile page's POST forms carry the active filter in their action's
// query string, which redirectTransactions reads back after a save. Rendered
// as a plain string after a literal "?", html/template percent-encoded the
// "&" and "=" (…?from%3D…%26to%3D…), so the server saw one bogus key and every
// filter was dropped on save. The value is now template.URL; this test renders
// the real page and checks the separators survive.
func TestAdminTransactionsPage_FilterQueryKeepsSeparators(t *testing.T) {
	conn := dbtest.Open(t, "test")
	uid := dbtest.BootstrapUserID(t, conn, "test")
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}

	req := httptest.NewRequest(http.MethodGet, "/admin/transactions?from=2026-06-01&to=2026-06-30&account_id=7", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.User{ID: uid, Username: "test", DisplayName: "Test", IsAdmin: true}))
	rec := httptest.NewRecorder()
	h.AdminTransactionsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Attribute context HTML-escapes "&" to "&amp;", which the browser decodes
	// back; the important part is that "=" and the separator are not
	// percent-encoded.
	const qs = "account_id=7&amp;from=2026-06-01&amp;to=2026-06-30"
	for _, want := range []string{`action="/admin/transactions/save?` + qs + `"`, `action="/admin/transactions?` + qs + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The action column header carries the edit-gated select-all box.
	if !strings.Contains(body, `<label class="te recon-del" title="Select every delete box on this page`) {
		t.Errorf("fills view missing the select-all header")
	}
	for _, bad := range []string{"%3D", "%3d", "%26"} {
		if strings.Contains(body, bad) {
			t.Errorf("filter query was percent-encoded (%q present)", bad)
		}
	}
}
