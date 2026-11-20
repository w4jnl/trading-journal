package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/models"
)

// A fill mail for a ref nobody owns, sent to a mailbox that is no user's
// address, cannot pick its owner; the admin claims it from the card. The
// click creates the account under them, flagged to confirm, with the firm
// guessed from the ref, and books the archived fills for that ref only.
func TestAccountsClaimEmailRef(t *testing.T) {
	fx := newPositionFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "mwmail", "testdata", "fill_real_mw.eml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("20260727T071933.000000000Z_aaaaaaaaaaaa.eml", string(raw))
	write("20260727T071935.000000000Z_cccccccccccc.eml", strings.Replace(string(raw), "LFE050-EXAMPLE1-TEST018", "APEX-999-1", 1))
	write("replay-state.log", "aaaaaaaaaaaa skipped_unknown_account\ncccccccccccc skipped_unknown_account\n")
	fx.h.MWEmailArchiveDir = dir

	post := func(ref string) *httptest.ResponseRecorder {
		form := url.Values{"ref": {ref}}
		req := httptest.NewRequest(http.MethodPost, "/accounts/email-fills/claim", strings.NewReader(form.Encode())).WithContext(fx.ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		fx.h.AccountsClaimEmailRef(rec, req)
		return rec
	}

	rec := post("LFE050-EXAMPLE1-TEST018")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/accounts?flash=created&mwfills=1" {
		t.Fatalf("claim = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	owners, err := fx.q.ListAccountsByRefAnyUser(fx.ctx, models.ListAccountsByRefAnyUserParams{ImportSource: "motivewave_csv", Ref: "LFE050-EXAMPLE1-TEST018"})
	if err != nil || len(owners) != 1 {
		t.Fatalf("owners = %v %v", owners, err)
	}
	a := owners[0]
	if a.UserID != fx.uid || a.NeedsSetup != 1 || a.Name != "LFE050-EXAMPLE1-TEST018" || a.Firm == nil || *a.Firm != "lucid" || a.Type == nil || *a.Type != "prop_eval" {
		t.Errorf("account = %+v", a)
	}
	execs, err := fx.q.ListExecutionsByAccount(fx.ctx, a.ID)
	if err != nil || len(execs) != 1 {
		t.Errorf("executions = %d %v, want the archived fill booked", len(execs), err)
	}
	// The other ref stays on the card, and a claimed ref cannot be claimed twice.
	var left []string
	for _, al := range fx.h.emailFillAlerts(fx.ctx) {
		left = append(left, al.Ref+":"+al.Status)
	}
	if strings.Join(left, ",") != "APEX-999-1:unknown" {
		t.Errorf("alerts after claim = %v", left)
	}
	if rec := post("LFE050-EXAMPLE1-TEST018"); !strings.Contains(rec.Header().Get("Location"), "createfail") {
		t.Errorf("second claim = %d %s, want createfail", rec.Code, rec.Header().Get("Location"))
	}
	if rec := post("NOPE-1"); !strings.Contains(rec.Header().Get("Location"), "createfail") {
		t.Errorf("unlisted claim = %d %s, want createfail", rec.Code, rec.Header().Get("Location"))
	}
}
