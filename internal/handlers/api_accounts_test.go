package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// closeFixturePosition sells the fixture's open ESM6 position so a closed
// trade (and its daily stats) exist.
func closeFixturePosition(t *testing.T, fx *positionFixture, price float64) {
	t.Helper()
	oid := "O-CLOSE"
	if _, err := fx.q.InsertExecution(fx.ctx, models.InsertExecutionParams{AccountID: fx.acct.ID, BrokerExecID: "P-CLOSE", OrderID: &oid, ImportMethod: "motivewave_csv",
		Symbol: "ESM6", SymbolRoot: "ES", Side: "sell", Qty: 3, Price: price, ExecTime: fx.t0.Add(20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.RebuildTradesForAccountSymbol(fx.ctx, fx.uid, fx.acct.ID, "ESM6"); err != nil {
		t.Fatal(err)
	}
}

func apiGet(ctx context.Context, fn http.HandlerFunc, path string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	fn(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// Accounts list with no anchor, then with one added through the API (a
// device key): the running balance is the anchor plus the net of the
// trades closed after it; equity follows; the anchor can be removed.
func TestAPIAccountsAndEquity(t *testing.T) {
	fx := newPositionFixture(t)
	// Exit at 7261 from an average of 7251: 10 points x 50 x 3 = 1500 gross.
	closeFixturePosition(t, fx, 7261)
	device := auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindDevice})
	agent := auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindAgent})

	code, out := apiGet(fx.ctx, fx.h.APIAccountsList, "/api/v1/accounts")
	if code != http.StatusOK {
		t.Fatalf("accounts = %d %v", code, out)
	}
	items := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	a := items[0].(map[string]any)
	if a["name"] != "pos-a" || a["balance"] != nil || a["firm"] != "apex" || a["active"] != true {
		t.Errorf("account = %v", a)
	}
	net, _ := a["dayNet"].(float64)

	post := func(ctx context.Context, acct string, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts/x/balances", strings.NewReader(body)).WithContext(ctx)
		req.SetPathValue("id", acct)
		rec := httptest.NewRecorder()
		fx.h.APIAccountBalanceAdd(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	acct := strconv.FormatInt(fx.acct.ID, 10)
	if code, _ := post(agent, acct, `{"balance": 50000}`); code != http.StatusForbidden {
		t.Errorf("agent anchor = %d", code)
	}
	if code, _ := post(device, acct, `{"note": "no balance"}`); code != http.StatusBadRequest {
		t.Errorf("missing balance = %d", code)
	}
	if code, _ := post(device, "999999", `{"balance": 1}`); code != http.StatusNotFound {
		t.Errorf("foreign account = %d", code)
	}
	// An anchor dated before the trade: the balance carries the trade's net.
	anchorDay := fx.t0.AddDate(0, 0, -2).Format("2006-01-02")
	code, out = post(device, acct, `{"date": "`+anchorDay+`", "balance": 50000, "note": "start"}`)
	if code != http.StatusCreated {
		t.Fatalf("anchor = %d %v", code, out)
	}
	bal := out["account"].(map[string]any)["balance"].(map[string]any)
	tradeNet := 0.0
	if trades, _ := fx.q.ListTradesForAccount(fx.ctx, fx.acct.ID); len(trades) == 1 && trades[0].NetPnl != nil {
		tradeNet = *trades[0].NetPnl
	}
	if bal["balance"] != 50000+tradeNet || bal["anchorDate"] != anchorDay || len(bal["anchors"].([]any)) != 1 {
		t.Errorf("balance = %v, want 50000 + %g", bal, tradeNet)
	}
	if net != 0 && tradeNet != net {
		t.Errorf("dayNet %g differs from the trade's net %g", net, tradeNet)
	}

	code, eq := apiGet(fx.ctx, fx.h.APIEquity, "/api/v1/equity")
	if code != http.StatusOK {
		t.Fatalf("equity = %d %v", code, eq)
	}
	if eq["equityNow"] != 50000+tradeNet || eq["included"] != 1.0 || eq["excluded"] != 0.0 {
		t.Errorf("equity = %v", eq)
	}
	dd := eq["drawdown"].(map[string]any)
	if _, ok := dd["maxDrawdown"]; !ok || dd["underwater"] != false {
		t.Errorf("drawdown = %v", dd)
	}
	if len(eq["byType"].([]any)) != 1 || len(eq["series"].([]any)) == 0 {
		t.Errorf("byType/series = %v / %v", eq["byType"], eq["series"])
	}
	if code, _ := apiGet(fx.ctx, fx.h.APIEquity, "/api/v1/equity?from=nope"); code != http.StatusBadRequest {
		t.Errorf("bad from = %d", code)
	}

	// Delete: wrong account 404, right one 204, then the balance is gone.
	anchorID := strconv.Itoa(int(bal["anchors"].([]any)[0].(map[string]any)["id"].(float64)))
	del := func(ctx context.Context, acct, anchor string) int {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/accounts/x/balances/y", nil).WithContext(ctx)
		req.SetPathValue("id", acct)
		req.SetPathValue("anchorId", anchor)
		rec := httptest.NewRecorder()
		fx.h.APIAccountBalanceDelete(rec, req)
		return rec.Code
	}
	if code := del(device, acct, "999"); code != http.StatusNotFound {
		t.Errorf("unknown anchor = %d", code)
	}
	if code := del(agent, acct, anchorID); code != http.StatusForbidden {
		t.Errorf("agent delete = %d", code)
	}
	if code := del(device, acct, anchorID); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if _, out := apiGet(fx.ctx, fx.h.APIAccountsList, "/api/v1/accounts"); out["items"].([]any)[0].(map[string]any)["balance"] != nil {
		t.Errorf("balance survived the delete")
	}
}

// The position row carries the tick data, the cycle's fills and the
// scale-in facts, and a device key's plan is the trader's.
func TestAPIPositionsTicksAndDevicePlan(t *testing.T) {
	fx := newPositionFixture(t)
	_, out := apiGet(fx.ctx, fx.h.APIPositionsList, "/api/v1/positions")
	p := out["items"].([]any)[0].(map[string]any)
	if p["tickSize"] != 0.25 || p["pointValue"] != 50.0 || p["tickValue"] != 12.5 || p["addedFills"] != 1.0 || p["addedQty"] != 1.0 || p["openPrice"] != 7250.0 || len(p["cycleFills"].([]any)) != 2 || p["rewardRisk"] != nil {
		t.Errorf("position = %v", p)
	}
	device := auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindDevice})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/positions/x/y/plan", strings.NewReader(`{"stop": 7241, "target": 7271}`)).WithContext(device)
	req.SetPathValue("account", strconv.FormatInt(fx.acct.ID, 10))
	req.SetPathValue("symbol", "ESM6")
	rec := httptest.NewRecorder()
	fx.h.APIPositionPlanSave(rec, req)
	var saved map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &saved)
	pos := saved["position"].(map[string]any)
	if rec.Code != http.StatusOK || pos["planSource"] != "trader" || pos["rewardRisk"] != 2.0 {
		t.Errorf("device plan = %d %v", rec.Code, pos)
	}
}

// The equity curve starts from the balance at the end of the day before
// its first day and adds each day's net once: a trade inside the range
// must not be counted in the starting balance as well.
func TestBalanceSeriesCountsTradesOnce(t *testing.T) {
	fx := newPositionFixture(t)
	closeFixturePosition(t, fx, 7261)
	anchorDay := time.Date(fx.t0.Year(), fx.t0.Month(), fx.t0.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -5)
	if err := fx.q.UpsertBalanceAnchor(fx.ctx, models.UpsertBalanceAnchorParams{AccountID: fx.acct.ID, AnchorDate: anchorDay, Balance: 50000}); err != nil {
		t.Fatal(err)
	}
	trades, _ := fx.q.ListTradesForAccount(fx.ctx, fx.acct.ID)
	net := *trades[0].NetPnl
	_, ys, now, _, included, _, err := fx.h.balanceSeriesForFilter(fx.ctx, accountFilter{}, nil, nil, time.UTC)
	if err != nil || included != 1 || len(ys) == 0 {
		t.Fatalf("series = %v (%v), included %d", ys, err, included)
	}
	if ys[len(ys)-1] != 50000+net || now != 50000+net {
		t.Errorf("equity = %g (last %g), want 50000 + %g: the trade counted twice", now, ys[len(ys)-1], net)
	}
	// The balance as of a day before the trade is the anchor alone.
	b, err := fx.h.loadBalances(fx.ctx, fx.t0.AddDate(0, 0, -1))
	if err != nil || b[fx.acct.ID].Balance != 50000 {
		t.Errorf("balance before the trade = %v (%v)", b[fx.acct.ID].Balance, err)
	}
}
