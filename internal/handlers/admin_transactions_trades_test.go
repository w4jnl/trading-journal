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
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// tradesFixture is the position-flip scenario from the builder tests, built
// through the real import service so trades and their fill links exist:
// buy 2 @ t0, sell 5 @ t5 (flip: closes the long 2, opens short 3), buy 3 @
// t10 (closes the short), sell 3 @ t15 (opens a short that stays open).
// Result: trade1 long 2 (t0-t5), trade2 short 3 (t5-t10), open short 3.
type tradesFixture struct {
	q      *models.Queries
	uid    int64
	acct   models.Account
	inline models.Account
	svc    *importer.ImportService
	h      *Handlers
	ctx    context.Context
	exec   map[string]int64 // broker id -> execution id (T1 buy2, T2 sell5, T3 buy3, T4 sell3)
	t0     time.Time
	trades []models.Trade // sorted by entry_time: [long2, short3]
	inlTrd models.Trade
}

func newTradesFixture(t *testing.T) *tradesFixture {
	t.Helper()
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")
	ctx := context.Background()
	firm := "apex"
	acct, err := q.CreateAccount(ctx, models.CreateAccountParams{UserID: uid, Name: "recon-a", Firm: &firm,
		ImportSource: "motivewave_csv", AccountRef: "APEX-TR-A", Currency: "USD", CommissionSource: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	fills := []struct {
		id, side string
		qty      float64
		price    float64
		at       time.Time
	}{
		{"T1", "buy", 2, 7250, t0}, {"T2", "sell", 5, 7252, t0.Add(5 * time.Minute)},
		{"T3", "buy", 3, 7251, t0.Add(10 * time.Minute)}, {"T4", "sell", 3, 7253, t0.Add(15 * time.Minute)},
	}
	execIDs := map[string]int64{}
	for _, f := range fills {
		if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: acct.ID, BrokerExecID: f.id, ImportMethod: "motivewave_csv",
			Symbol: "ESM6", SymbolRoot: "ES", Side: f.side, Qty: f.qty, Price: f.price, ExecTime: f.at}); err != nil {
			t.Fatalf("insert %s: %v", f.id, err)
		}
		e, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{AccountID: acct.ID, BrokerExecID: f.id})
		if err != nil {
			t.Fatal(err)
		}
		execIDs[f.id] = e.ID
	}
	svc := &importer.ImportService{DB: conn, Queries: q, Timezone: time.UTC, UserID: uid}
	if err := svc.RebuildTradesForAccountSymbol(ctx, uid, acct.ID, "ESM6"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	trades, err := q.ListTradesForAccount(ctx, acct.ID)
	if err != nil || len(trades) != 2 {
		t.Fatalf("trades = %d (%v), want 2", len(trades), err)
	}
	// Inline (trades-CSV) account with one imported trade: read-only everywhere.
	tsx := "topstep"
	inline, err := q.CreateAccount(ctx, models.CreateAccountParams{UserID: uid, Name: "recon-inline", Firm: &tsx,
		ImportSource: "topstepx_trades_csv", AccountRef: "EXPRESS-TR", Currency: "USD", CommissionSource: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	exit, exitPrice, net, bid := t0.Add(2*time.Minute), 7005.0, 250.0, "B1"
	inlTrd, err := q.CreateImportedTrade(ctx, models.CreateImportedTradeParams{AccountID: inline.ID, Symbol: "ESM6", SymbolRoot: "ES",
		Direction: "long", Qty: 1, EntryPrice: 7000, ExitPrice: &exitPrice, EntryTime: t0, ExitTime: &exit, NetPnl: &net, BrokerTradeID: &bid})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, ImportSvc: svc}
	uctx := auth.WithUser(ctx, &auth.User{ID: uid, Username: "test", DisplayName: "Test", IsAdmin: true})
	return &tradesFixture{q: q, uid: uid, acct: acct, inline: inline, svc: svc, h: h, ctx: uctx, exec: execIDs, t0: t0, trades: trades, inlTrd: inlTrd}
}

func (fx *tradesFixture) page(t *testing.T, target string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(fx.ctx)
	rec := httptest.NewRecorder()
	fx.h.AdminTransactionsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func (fx *tradesFixture) save(t *testing.T, target string, form url.Values) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode())).WithContext(fx.ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	fx.h.AdminTransactionsTradesSave(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST %s = %d: %s", target, rec.Code, rec.Body.String())
	}
	return rec.Result().Header.Get("Location")
}

func (fx *tradesFixture) execRow(t *testing.T, brokerID string) models.Execution {
	t.Helper()
	e, err := fx.q.GetExecutionByBrokerID(context.Background(), models.GetExecutionByBrokerIDParams{AccountID: fx.acct.ID, BrokerExecID: brokerID})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// rowTag returns the <tr ...> opening tag of the trade's row in the rendered page.
func rowTag(body string, tradeID int64) string {
	marker := `data-trade-id="` + itoa64(tradeID) + `"`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(body[:i], "<tr")
	end := strings.Index(body[i:], ">")
	return body[start : i+end+1]
}

// The trades view renders one row per closed trade with its warnings, keeps
// the filter (and view) on its form action and toggle links, and offers no
// delete on read-only accounts.
func TestTradesPage_RendersTradesView(t *testing.T) {
	fx := newTradesFixture(t)
	body := fx.page(t, "/admin/transactions?view=trades&from=2026-05-01&to=2026-05-31")
	for _, want := range []string{
		`action="/admin/transactions/trades/save?from=2026-05-01&amp;to=2026-05-31&amp;view=trades"`,
		`href="/admin/transactions?from=2026-05-01&amp;to=2026-05-31"`, // Fills toggle: no view
		`href="/admin/transactions?from=2026-05-01&amp;to=2026-05-31&amp;view=trades"`,
		`name="trade.` + itoa64(fx.trades[0].ID) + `.delete"`,
		`name="trade.` + itoa64(fx.trades[0].ID) + `.sig" value="` + tradeSig(fx.trades[0].EntryTime) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	t1, t2 := fx.trades[0], fx.trades[1]
	if tag := rowTag(body, t1.ID); !strings.Contains(tag, `data-flip="#`+itoa64(t2.ID)+` "`) || !strings.Contains(tag, `data-fills="2"`) {
		t.Errorf("trade1 row = %q (want flip link to %d, 2 fills)", tag, t2.ID)
	}
	if tag := rowTag(body, t2.ID); !strings.Contains(tag, `data-flip="#`+itoa64(t1.ID)+` "`) {
		t.Errorf("trade2 row = %q (want flip link to %d)", tag, t1.ID)
	}
	if !strings.Contains(body, `data-trade-id="`+itoa64(fx.inlTrd.ID)+`"`) {
		t.Errorf("inline trade not listed")
	}
	// Fills expand into a full-width row under the trade, toggled per row; the
	// delete checkbox is gated behind Edit like the fills view (.te class).
	for _, want := range []string{
		`data-target="fills-` + itoa64(t1.ID) + `"`,
		`<tr class="recon-fills-row" id="fills-` + itoa64(t1.ID) + `" hidden>`,
		`<td colspan="11">`,
		`<label class="te recon-del"><input type="checkbox" form="trade-form" name="trade.` + itoa64(t1.ID) + `.delete">`,
		`id="recon-trades-edit"`, `id="recon-fills-all"`, `class="table-scroll recon-editable"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, `name="trade.`+itoa64(fx.inlTrd.ID)+`.delete"`) {
		t.Errorf("inline trade must not offer delete")
	}
	if strings.Contains(body, "%3D") || strings.Contains(body, "%26") {
		t.Errorf("filter query percent-encoded in trades view")
	}
}

// Deleting a trade tombstones exactly its fills under one group (the shared
// flip fill included), the neighbour is rebuilt from what remains, and the
// redirect keeps filter + view. Restoring the group brings both trades back.
func TestTradesSave_DeleteThenRestoreGroup(t *testing.T) {
	fx := newTradesFixture(t)
	t1 := fx.trades[0]
	target := "/admin/transactions/trades/save?view=trades&from=2026-05-01&to=2026-05-31"
	form := url.Values{}
	form.Set("trade."+itoa64(t1.ID)+".delete", "on")
	form.Set("trade."+itoa64(t1.ID)+".sig", tradeSig(t1.EntryTime))
	loc := fx.save(t, target, form)
	for _, want := range []string{"flash=deleted+1+trade", "2+fills", "view=trades", "from=2026-05-01", "to=2026-05-31"} {
		if !strings.Contains(loc, want) {
			t.Errorf("redirect %q missing %q", loc, want)
		}
	}
	buy2, sell5, buy3, sell3 := fx.execRow(t, "T1"), fx.execRow(t, "T2"), fx.execRow(t, "T3"), fx.execRow(t, "T4")
	if buy2.DeletedAt == nil || sell5.DeletedAt == nil || buy2.DeletedGroup == nil || sell5.DeletedGroup == nil {
		t.Fatalf("trade1 fills not tombstoned: %+v %+v", buy2, sell5)
	}
	if *buy2.DeletedGroup != *sell5.DeletedGroup || !strings.HasPrefix(*buy2.DeletedGroup, "tg_") {
		t.Errorf("group ids = %q / %q", *buy2.DeletedGroup, *sell5.DeletedGroup)
	}
	if buy3.DeletedAt != nil || sell3.DeletedAt != nil {
		t.Errorf("neighbour fills must stay live")
	}
	after, _ := fx.q.ListTradesForAccount(context.Background(), fx.acct.ID)
	if len(after) != 1 || after[0].Direction != "long" || after[0].Qty != 3 || !after[0].EntryTime.Equal(fx.t0.Add(10*time.Minute)) {
		t.Fatalf("trades after delete = %+v, want one long 3 from t10", after)
	}

	groups, err := fx.h.listDeletedTradeGroups(fx.ctx, fx.uid, txnFilter{})
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
	g := groups[0]
	if g.FillCount != 2 || g.BuyQty != 2 || g.SellQty != 5 || !g.First.Equal(fx.t0) || !g.Last.Equal(fx.t0.Add(5*time.Minute)) || g.Symbol != "ESM6" {
		t.Errorf("group = %+v", g)
	}
	// With show-deleted on, the deleted trade is a dimmed row IN the trades
	// table at its chronological position (newest first: the remaining trade
	// starts at t10, the deleted one at t0, so it comes after), not a
	// separate section nobody scrolls to.
	body := fx.page(t, "/admin/transactions?view=trades&deleted=1")
	if !strings.Contains(body, `name="group.`+g.GroupID+`.restore"`) {
		t.Errorf("restore checkbox for %s not rendered", g.GroupID)
	}
	rowIdx := strings.Index(body, `<tr class="txn-item--deleted" data-group-id="`+g.GroupID+`"`)
	liveIdx := strings.Index(body, `data-trade-id="`+itoa64(after[0].ID)+`"`)
	if rowIdx < 0 || liveIdx < 0 || rowIdx < liveIdx {
		t.Errorf("deleted row must be inline after the newer live trade (deleted at %d, live at %d)", rowIdx, liveIdx)
	}
	if strings.Contains(body, "Deleted trades</strong>") {
		t.Errorf("separate deleted-trades section should be gone")
	}
	if !strings.Contains(body, "+ 1 deleted") {
		t.Errorf("header should count the deleted trade")
	}

	form = url.Values{}
	form.Set("group."+g.GroupID+".restore", "on")
	loc = fx.save(t, target, form)
	if !strings.Contains(loc, "flash=restored+1+trade") {
		t.Errorf("restore redirect = %q", loc)
	}
	for _, id := range []string{"T1", "T2"} {
		e := fx.execRow(t, id)
		if e.DeletedAt != nil || e.DeletedGroup != nil {
			t.Errorf("%s not restored: %+v", id, e)
		}
	}
	restored, _ := fx.q.ListTradesForAccount(context.Background(), fx.acct.ID)
	if len(restored) != 2 || !restored[0].EntryTime.Equal(fx.t0) || !restored[1].EntryTime.Equal(fx.t0.Add(5*time.Minute)) {
		t.Errorf("trades after restore = %+v", restored)
	}
	if groups, _ := fx.h.listDeletedTradeGroups(fx.ctx, fx.uid, txnFilter{}); len(groups) != 0 {
		t.Errorf("groups after restore = %+v", groups)
	}
}

// A per-fill restore in the fills view detaches that fill from its group; the
// group keeps listing (and restoring) the remainder.
func TestTradesSave_PerFillRestoreDetaches(t *testing.T) {
	fx := newTradesFixture(t)
	t1 := fx.trades[0]
	form := url.Values{}
	form.Set("trade."+itoa64(t1.ID)+".delete", "on")
	form.Set("trade."+itoa64(t1.ID)+".sig", tradeSig(t1.EntryTime))
	fx.save(t, "/admin/transactions/trades/save", form)

	bulk := url.Values{}
	bulk.Set("txn."+itoa64(fx.exec["T1"])+".restore", "on")
	req := httptest.NewRequest(http.MethodPost, "/admin/transactions/save", strings.NewReader(bulk.Encode())).WithContext(fx.ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fx.h.AdminTransactionsBulkSave(httptest.NewRecorder(), req)

	if e := fx.execRow(t, "T1"); e.DeletedAt != nil || e.DeletedGroup != nil {
		t.Errorf("per-fill restore must clear both columns: %+v", e)
	}
	groups, _ := fx.h.listDeletedTradeGroups(fx.ctx, fx.uid, txnFilter{})
	if len(groups) != 1 || groups[0].FillCount != 1 {
		t.Errorf("groups = %+v, want one group with 1 remaining fill", groups)
	}
}

// Read-only accounts, vanished ids, stale fingerprints and other users' trades
// are all refused with a reload/read-only message and change nothing.
func TestTradesSave_Refusals(t *testing.T) {
	fx := newTradesFixture(t)
	ctx := context.Background()
	t1 := fx.trades[0]

	cases := map[string]struct {
		id   int64
		sig  string
		want string
	}{
		"inline account": {fx.inlTrd.ID, tradeSig(fx.inlTrd.EntryTime), "read-only"},
		"vanished id":    {999999, "x", "no+longer+exists"},
		"stale sig":      {t1.ID, "2000-01-01T00:00:00Z", "changed+since"},
	}
	for name, c := range cases {
		form := url.Values{}
		form.Set("trade."+itoa64(c.id)+".delete", "on")
		form.Set("trade."+itoa64(c.id)+".sig", c.sig)
		loc := fx.save(t, "/admin/transactions/trades/save?view=trades", form)
		if !strings.Contains(loc, "err=") || !strings.Contains(loc, c.want) {
			t.Errorf("%s: redirect %q, want err containing %q", name, loc, c.want)
		}
	}
	for _, id := range []string{"T1", "T2", "T3", "T4"} {
		if e := fx.execRow(t, id); e.DeletedAt != nil {
			t.Errorf("%s was tombstoned by a refused request", id)
		}
	}
	if trades, _ := fx.q.ListTradesForAccount(ctx, fx.acct.ID); len(trades) != 2 {
		t.Errorf("trades = %d, want 2 untouched", len(trades))
	}

	// Another user's trade: not found for this user, intact for the owner.
	bob, _ := fx.q.CreateUser(ctx, models.CreateUserParams{Username: "bob"})
	bAcct, _ := fx.q.CreateAccount(ctx, models.CreateAccountParams{UserID: bob.ID, Name: "bob-a", ImportSource: "motivewave_csv", AccountRef: "BOB-1", Currency: "USD", CommissionSource: "rate"})
	exit, px, bid := fx.t0.Add(time.Minute), 7001.0, "BOB-T"
	bTrade, err := fx.q.CreateImportedTrade(ctx, models.CreateImportedTradeParams{AccountID: bAcct.ID, Symbol: "ESM6", SymbolRoot: "ES", Direction: "long", Qty: 1,
		EntryPrice: 7000, ExitPrice: &px, EntryTime: fx.t0, ExitTime: &exit, BrokerTradeID: &bid})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("trade."+itoa64(bTrade.ID)+".delete", "on")
	form.Set("trade."+itoa64(bTrade.ID)+".sig", tradeSig(bTrade.EntryTime))
	if loc := fx.save(t, "/admin/transactions/trades/save", form); !strings.Contains(loc, "no+longer+exists") {
		t.Errorf("other user's trade: %q", loc)
	}
	if bt, _ := fx.q.ListTradesForAccount(ctx, bAcct.ID); len(bt) != 1 {
		t.Errorf("bob's trade was touched")
	}
}

// Journal links are surfaced as warnings and, as documented, a deleted trade's
// trade-scope entry is left detached while its group links are dropped.
func TestTradesSave_JournalLinksWarnAndDetach(t *testing.T) {
	fx := newTradesFixture(t)
	ctx := context.Background()
	t1 := fx.trades[0]
	je, err := fx.q.CreateJournalEntry(ctx, models.CreateJournalEntryParams{UserID: &fx.uid, TradeID: &t1.ID, AccountID: &fx.acct.ID,
		EntryDate: fx.t0, Body: "note", Scope: "trade"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.h.DB.ExecContext(ctx, "INSERT INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)", je.ID, t1.ID); err != nil {
		t.Fatal(err)
	}
	body := fx.page(t, "/admin/transactions?view=trades")
	if tag := rowTag(body, t1.ID); !strings.Contains(tag, `data-journal="1"`) || !strings.Contains(tag, `data-groups="1"`) {
		t.Errorf("row = %q, want journal + group flags", tag)
	}
	form := url.Values{}
	form.Set("trade."+itoa64(t1.ID)+".delete", "on")
	form.Set("trade."+itoa64(t1.ID)+".sig", tradeSig(t1.EntryTime))
	fx.save(t, "/admin/transactions/trades/save", form)
	var tradeID *int64
	if err := fx.h.DB.QueryRowContext(ctx, "SELECT trade_id FROM journal_entries WHERE id = ?", je.ID).Scan(&tradeID); err != nil {
		t.Fatal(err)
	}
	if tradeID != nil {
		t.Errorf("journal entry still points at trade %d after delete", *tradeID)
	}
	var links int
	_ = fx.h.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM journal_entry_trades WHERE journal_entry_id = ?", je.ID).Scan(&links)
	if links != 0 {
		t.Errorf("group links = %d, want 0", links)
	}
}
