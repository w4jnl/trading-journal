package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// positionFixture: one account long 3 ESM6 (buy 2 @ 7250, buy 1 @ 7253:
// average 7251), still open.
type positionFixture struct {
	h    *Handlers
	q    *models.Queries
	svc  *importer.ImportService
	ctx  context.Context
	uid  int64
	acct models.Account
	t0   time.Time
}

func newPositionFixture(t *testing.T) *positionFixture {
	t.Helper()
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")
	ctx := context.Background()
	firm := "apex"
	acct, err := q.CreateAccount(ctx, models.CreateAccountParams{UserID: uid, Name: "pos-a", Firm: &firm,
		ImportSource: "motivewave_csv", AccountRef: "APEX-POS-A", Currency: "USD", CommissionSource: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	oid1, oid2 := "O-1", "O-2"
	for _, f := range []struct {
		id, oid, side string
		qty, price    float64
		at            time.Time
	}{{"P1", oid1, "buy", 2, 7250, t0}, {"P2", oid2, "buy", 1, 7253, t0.Add(2 * time.Minute)}} {
		oid := f.oid
		if _, err := q.InsertExecution(ctx, models.InsertExecutionParams{AccountID: acct.ID, BrokerExecID: f.id, OrderID: &oid, ImportMethod: "motivewave_csv",
			Symbol: "ESM6", SymbolRoot: "ES", Side: f.side, Qty: f.qty, Price: f.price, ExecTime: f.at}); err != nil {
			t.Fatalf("insert %s: %v", f.id, err)
		}
	}
	svc := &importer.ImportService{DB: conn, Queries: q, Timezone: time.UTC, UserID: uid}
	h := &Handlers{DB: conn, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC, ImportSvc: svc, LiveData: livedata.NewNotifier()}
	uctx := auth.WithUser(ctx, &auth.User{ID: uid, Username: "test", DisplayName: "Test", IsAdmin: true})
	return &positionFixture{h: h, q: q, svc: svc, ctx: uctx, uid: uid, acct: acct, t0: t0}
}

func (fx *positionFixture) sheet(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/positions/x/y", nil).WithContext(fx.ctx)
	req.SetPathValue("account", strconv.FormatInt(fx.acct.ID, 10))
	req.SetPathValue("symbol", "ESM6")
	rec := httptest.NewRecorder()
	fx.h.PositionSheet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET sheet = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func (fx *positionFixture) savePlan(t *testing.T, form url.Values) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/positions/x/y/plan", strings.NewReader(form.Encode())).WithContext(fx.ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("account", strconv.FormatInt(fx.acct.ID, 10))
	req.SetPathValue("symbol", "ESM6")
	rec := httptest.NewRecorder()
	fx.h.PositionPlanSave(rec, req)
	return rec.Code, rec.Body.String()
}

func (fx *positionFixture) trades(t *testing.T, query string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/trades"+query, nil).WithContext(WithNonce(fx.ctx, "n"))
	rec := httptest.NewRecorder()
	fx.h.TradesPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /trades%s = %d: %s", query, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// The open position is a row on /trades and a sheet with a plan form; the
// plan derives risk from the stop at the position's size, shows on the row,
// and the status filter picks open, closed or both.
func TestOpenPosition_SheetAndPlan(t *testing.T) {
	fx := newPositionFixture(t)

	body := fx.sheet(t)
	for _, want := range []string{`data-sheet-title="ESM6 · open ↑ 3"`, `name="stop"`, `name="thesis"`, "no plan recorded yet", `data-plan-entry="7251"`, `data-plan-qty="3"`, "Fills of this position"} {
		if !strings.Contains(body, want) {
			t.Errorf("sheet lacks %q", want)
		}
	}

	page := fx.trades(t, "")
	for _, want := range []string{`data-trade-id="pos:` + strconv.FormatInt(fx.acct.ID, 10) + `:ESM6"`, `hx-get="/positions/` + strconv.FormatInt(fx.acct.ID, 10) + `/ESM6"`, "set plan", "Open now", `name="status"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/trades lacks %q", want)
		}
	}
	if strings.Contains(fx.trades(t, "?status=closed"), "Open now") {
		t.Errorf("status=closed still shows the open strip")
	}
	if open := fx.trades(t, "?status=open"); !strings.Contains(open, "Open now") || strings.Contains(open, `id="sel-all"`) {
		t.Errorf("status=open must show the strip without the trades table")
	}
	if strings.Contains(fx.trades(t, "?symbol_root=NQ"), "Open now") {
		t.Errorf("the symbol filter must narrow the strip")
	}

	// Stop only: risk follows at the position's size. |7251 - 7241| * 50 * 3 = 1500.
	code, out := fx.savePlan(t, url.Values{"stop": {"7241"}, "risk": {""}, "target": {"7271"}, "thesis": {"breakout over the open"}})
	if code != http.StatusOK {
		t.Fatalf("save = %d: %s", code, out)
	}
	for _, want := range []string{`value="7241"`, `value="1500.00"`, `value="7271"`, `value="breakout over the open"`, "reward:risk <strong class=\"num\">2.0</strong>"} {
		if !strings.Contains(out, want) {
			t.Errorf("sheet after save lacks %q", want)
		}
	}
	plans, err := fx.q.ListOpenPositionPlansForUser(context.Background(), fx.uid)
	if err != nil || len(plans) != 1 || plans[0].Stop == nil || *plans[0].Stop != 7241 || plans[0].Risk == nil || *plans[0].Risk != 1500 || plans[0].Thesis != "breakout over the open" {
		t.Fatalf("plans = %+v (%v)", plans, err)
	}
	page = fx.trades(t, "")
	if !strings.Contains(page, "7241") || !strings.Contains(page, "1500.00") || strings.Contains(page, "set plan") {
		t.Errorf("/trades row does not show the plan")
	}

	// Risk typed, stop untouched: the stop follows the risk (snapped to tick).
	// 750 / (50 * 3) = 5 points -> 7246.
	code, out = fx.savePlan(t, url.Values{"stop": {"7241"}, "risk": {"750"}, "target": {"7271"}, "thesis": {"breakout over the open"}})
	if code != http.StatusOK || !strings.Contains(out, `value="7246"`) {
		t.Fatalf("risk edit: %d, stop not derived: %s", code, out)
	}

	// Scale in: the stored risk follows the stop at the new size on the next
	// render. |7252 - 7246| * 50 * 4 = 1200 (avg of 2@7250, 1@7253, 1@7255).
	oid := "O-3"
	if _, err := fx.q.InsertExecution(context.Background(), models.InsertExecutionParams{AccountID: fx.acct.ID, BrokerExecID: "P3", OrderID: &oid, ImportMethod: "motivewave_csv",
		Symbol: "ESM6", SymbolRoot: "ES", Side: "buy", Qty: 1, Price: 7255, ExecTime: fx.t0.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if body := fx.sheet(t); !strings.Contains(body, `value="1200.00"`) {
		t.Errorf("risk after scale-in not recomputed: %s", body)
	}

	// Empty form clears the plan.
	if code, _ := fx.savePlan(t, url.Values{"stop": {""}, "risk": {""}, "target": {""}, "thesis": {""}}); code != http.StatusOK {
		t.Fatalf("clear = %d", code)
	}
	if plans, _ := fx.q.ListOpenPositionPlansForUser(context.Background(), fx.uid); len(plans) != 0 {
		t.Errorf("plan not deleted: %+v", plans)
	}
}

// Closing the position hands the plan to the trade: the trade carries stop,
// target and the risk at its own entry and size; the sheet reports the
// position flat and points at the trade; the strip row is gone.
func TestOpenPosition_PlanFollowsIntoTrade(t *testing.T) {
	fx := newPositionFixture(t)
	if code, out := fx.savePlan(t, url.Values{"stop": {"7241"}, "risk": {""}, "target": {"7271"}, "thesis": {"t"}}); code != http.StatusOK {
		t.Fatalf("save = %d: %s", code, out)
	}
	oid := "O-4"
	if _, err := fx.q.InsertExecution(context.Background(), models.InsertExecutionParams{AccountID: fx.acct.ID, BrokerExecID: "P4", OrderID: &oid, ImportMethod: "motivewave_csv",
		Symbol: "ESM6", SymbolRoot: "ES", Side: "sell", Qty: 3, Price: 7265, ExecTime: fx.t0.Add(20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.RebuildTradesForAccountSymbol(context.Background(), fx.uid, fx.acct.ID, "ESM6"); err != nil {
		t.Fatal(err)
	}
	trades, err := fx.q.ListTradesForAccount(context.Background(), fx.acct.ID)
	if err != nil || len(trades) != 1 {
		t.Fatalf("trades = %d (%v)", len(trades), err)
	}
	tr := trades[0]
	if tr.PlannedStop == nil || *tr.PlannedStop != 7241 || tr.PlannedTarget == nil || *tr.PlannedTarget != 7271 || tr.PlannedRisk == nil || *tr.PlannedRisk != 1500 {
		t.Fatalf("trade plan = %v / %v / %v, want 7241 / 1500 / 7271", tr.PlannedStop, tr.PlannedRisk, tr.PlannedTarget)
	}
	if plans, _ := fx.q.ListOpenPositionPlansForUser(context.Background(), fx.uid); len(plans) != 0 {
		t.Errorf("handed-off plan still listed as open: %+v", plans)
	}
	body := fx.sheet(t)
	if !strings.Contains(body, "flat now") || !strings.Contains(body, `hx-get="/trades/`+strconv.FormatInt(tr.ID, 10)+`"`) {
		t.Errorf("flat sheet should point at trade %d: %s", tr.ID, body)
	}
	if page := fx.trades(t, ""); strings.Contains(page, "Open now") || !strings.Contains(page, `data-trade-id="`+strconv.FormatInt(tr.ID, 10)+`"`) {
		t.Errorf("after close the strip must be gone and the trade listed")
	}
	// A plan on a flat position is refused.
	if code, _ := fx.savePlan(t, url.Values{"stop": {"7241"}}); code != http.StatusConflict {
		t.Errorf("plan on a flat position = %d, want 409", code)
	}
}

// Another user's account is not found, not planned.
func TestOpenPosition_OtherUser(t *testing.T) {
	fx := newPositionFixture(t)
	other := auth.WithUser(context.Background(), &auth.User{ID: fx.uid + 1, Username: "other"})
	req := httptest.NewRequest(http.MethodGet, "/positions/x/y", nil).WithContext(other)
	req.SetPathValue("account", strconv.FormatInt(fx.acct.ID, 10))
	req.SetPathValue("symbol", "ESM6")
	rec := httptest.NewRecorder()
	fx.h.PositionSheet(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("other user's sheet = %d, want 404", rec.Code)
	}
}
