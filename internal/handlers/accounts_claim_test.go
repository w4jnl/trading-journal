package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/mwmail"
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
	// Opening balance 0 dated before the archived fill, so the row shows a
	// balance at once and the booked fill counts toward it.
	anchors, err := fx.q.ListBalanceAnchors(fx.ctx, fx.uid)
	var seeded bool
	for _, an := range anchors {
		if an.AccountID == a.ID && an.Balance == 0 && an.AnchorDate.Before(execs[0].ExecTime) {
			seeded = true
		}
	}
	if err != nil || !seeded {
		t.Errorf("opening balance not seeded: %+v %v", anchors, err)
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

// Strict addresses refused a capture sent to the second user's fill address
// for the fixture user's account: the card names both and lists the fills
// as "mismatch"; an accepted mismatch noted by the importer shows too.
func TestEmailFillAlertsMismatch(t *testing.T) {
	fx := newPositionFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "mwmail", "testdata", "fill_real_mw.eml"))
	if err != nil {
		t.Fatal(err)
	}
	to := mwmail.Parse(raw).To
	second, err := fx.q.CreateUser(fx.ctx, models.CreateUserParams{Username: "second", Email: nil, PasswordHash: nil, IsAdmin: 0, DisplayName: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.q.SetUserMWFillAddress(fx.ctx, models.SetUserMWFillAddressParams{MwFillAddress: &to, ID: second.ID}); err != nil {
		t.Fatal(err)
	}
	firm := "lucid"
	if _, err := fx.q.CreateAccount(fx.ctx, models.CreateAccountParams{UserID: fx.uid, Name: "lucid-18", Firm: &firm, ImportSource: "motivewave_csv",
		AccountRef: "LFE050-EXAMPLE1-TEST018", Currency: "USD", CommissionSource: "rate"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "20260727T071933.000000000Z_aaaaaaaaaaaa.eml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "replay-state.log"), []byte("aaaaaaaaaaaa skipped_address_mismatch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.h.MWEmailArchiveDir = dir

	alerts := fx.h.emailFillAlerts(fx.ctx)
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v", alerts)
	}
	a := alerts[0]
	if a.Status != "mismatch" || a.Count != 1 || a.OwnerName != "lucid-18" || a.OwnerUser != "test" || a.AddressedUser != "second" {
		t.Errorf("alert = %+v", a)
	}

	// An accepted mismatch for another ref the fixture user owns, booked by
	// the importer with strict addresses off and noted since start.
	if _, err := fx.q.CreateAccount(fx.ctx, models.CreateAccountParams{UserID: fx.uid, Name: "apex-1", Firm: &firm, ImportSource: "motivewave_csv",
		AccountRef: "APEX-999-1", Currency: "USD", CommissionSource: "rate"}); err != nil {
		t.Fatal(err)
	}
	f := importer.Fill{AccountRef: "APEX-999-1", BrokerExecID: "MWEMAIL-1", OrderID: "1", Symbol: "ESM6", SymbolRoot: "ES",
		Side: "buy", Qty: 1, Price: 7000, ExecTime: fx.t0, Recipient: to}
	if action, err := fx.svc.IngestEmailFill(fx.ctx, f); err != nil || action != importer.EmailInserted {
		t.Fatalf("ingest = %q %v", action, err)
	}
	alerts = fx.h.emailFillAlerts(fx.ctx)
	if len(alerts) != 2 || alerts[1].Status != "mismatch-accepted" || alerts[1].AddressedUser != "second" || alerts[1].OwnerUser != "test" {
		t.Errorf("alerts = %+v", alerts)
	}
}

// Confirm on an auto-created row clears its flag in one click, only for the
// owner and only once firm and type are set.
func TestAccountsConfirm(t *testing.T) {
	fx := newPositionFixture(t)
	post := func(id int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/accounts/x/confirm", nil).WithContext(fx.ctx)
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		rec := httptest.NewRecorder()
		fx.h.AccountsConfirm(rec, req)
		return rec
	}
	created, err := fx.q.CreateAccount(fx.ctx, importer.EmailAccountParams(fx.uid, "APEX-777-1", "Rithmic WS"))
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(created.ID); rec.Header().Get("Location") != "/accounts?flash=confirmed#row-"+strconv.FormatInt(created.ID, 10) {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if a, err := fx.q.GetAccountByID(fx.ctx, models.GetAccountByIDParams{ID: created.ID, UserID: fx.uid}); err != nil || a.NeedsSetup != 0 {
		t.Errorf("after confirm: %+v %v", a, err)
	}
	// No firm: refused, flag kept.
	bare, err := fx.q.CreateAccount(fx.ctx, models.CreateAccountParams{UserID: fx.uid, Name: "120099", ImportSource: "motivewave_csv", AccountRef: "120099", Currency: "USD", NeedsSetup: 1, CommissionSource: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(bare.ID); !strings.Contains(rec.Header().Get("Location"), "createfail") {
		t.Errorf("bare confirm = %s", rec.Header().Get("Location"))
	}
	// Another user's account: not found, nothing changes.
	other, err := fx.q.CreateUser(fx.ctx, models.CreateUserParams{Username: "other"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := fx.q.CreateAccount(fx.ctx, importer.EmailAccountParams(other.ID, "APEX-777-2", "Rithmic WS"))
	if err != nil {
		t.Fatal(err)
	}
	post(theirs.ID)
	if a, err := fx.q.GetAccountByID(fx.ctx, models.GetAccountByIDParams{ID: theirs.ID, UserID: other.ID}); err != nil || a.NeedsSetup != 1 {
		t.Errorf("other user's account touched: %+v %v", a, err)
	}
}

// The accounts list shows each account's last fill and sorts by it, by
// balance or by name; accounts to confirm stay first whatever the sort.
func TestAccountsPageSort(t *testing.T) {
	fx := newPositionFixture(t)
	firm := "apex"
	if _, err := fx.q.CreateAccount(fx.ctx, models.CreateAccountParams{UserID: fx.uid, Name: "aaa-idle", Firm: &firm, ImportSource: "motivewave_csv",
		AccountRef: "APEX-1-1", Currency: "USD", CommissionSource: "rate"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.q.CreateAccount(fx.ctx, importer.EmailAccountParams(fx.uid, "ZZZ-NEW", "Rithmic WS")); err != nil {
		t.Fatal(err)
	}
	page := func(query string) string {
		req := httptest.NewRequest(http.MethodGet, "/accounts"+query, nil).WithContext(WithNonce(fx.ctx, "n"))
		rec := httptest.NewRecorder()
		fx.h.AccountsPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", query, rec.Code)
		}
		return rec.Body.String()
	}
	order := func(body string) []string {
		var out []string
		for _, n := range []string{"ZZZ-NEW", "aaa-idle", "pos-a"} {
			out = append(out, n)
		}
		sort.Slice(out, func(i, j int) bool { return strings.Index(body, ">"+out[i]+"<") < strings.Index(body, ">"+out[j]+"<") })
		return out
	}
	body := page("")
	if !strings.Contains(body, fx.t0.In(fx.h.localTZ()).Format("2006-01-02")) {
		t.Errorf("last fill date missing from the page")
	}
	if got := order(body); strings.Join(got, ",") != "ZZZ-NEW,aaa-idle,pos-a" {
		t.Errorf("name asc = %v", got)
	}
	if got := order(page("?sort=last&dir=desc")); strings.Join(got, ",") != "ZZZ-NEW,pos-a,aaa-idle" {
		t.Errorf("last desc = %v", got)
	}
	if got := order(page("?sort=name&dir=desc")); strings.Join(got, ",") != "ZZZ-NEW,pos-a,aaa-idle" {
		t.Errorf("name desc = %v", got)
	}
	if !strings.Contains(page("?sort=last"), `aria-sort="descending"`) {
		t.Errorf("active sort header not marked")
	}
}
