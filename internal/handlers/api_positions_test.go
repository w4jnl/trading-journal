package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The API lists the open position with its plan and writes a plan with
// source 'api'; the sheet then shows it as set through the API. A flat
// contract and another user's account are refused.
func TestAPIPositions(t *testing.T) {
	fx := newPositionFixture(t)
	get := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/positions", nil).WithContext(fx.ctx)
		rec := httptest.NewRecorder()
		fx.h.APIPositionsList(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET positions = %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	items := get()["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	p := items[0].(map[string]any)
	if p["symbol"] != "ESM6" || p["direction"] != "long" || p["qty"] != 3.0 || p["avgEntry"] != 7251.0 || p["stop"] != nil || p["planSource"] != nil {
		t.Errorf("position = %v", p)
	}

	post := func(account, symbol, body string, ctx context.Context) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/positions/x/y/plan", strings.NewReader(body)).WithContext(ctx)
		req.SetPathValue("account", account)
		req.SetPathValue("symbol", symbol)
		rec := httptest.NewRecorder()
		fx.h.APIPositionPlanSave(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	acct := strconv.FormatInt(fx.acct.ID, 10)
	code, out := post(acct, "ESM6", `{"stop": 7241, "target": 7271, "thesis": "api plan"}`, fx.ctx)
	if code != http.StatusOK {
		t.Fatalf("POST plan = %d: %v", code, out)
	}
	pos := out["position"].(map[string]any)
	if pos["stop"] != 7241.0 || pos["risk"] != 1500.0 || pos["target"] != 7271.0 || pos["planSource"] != "api" || pos["thesis"] != "api plan" {
		t.Errorf("saved = %v", pos)
	}
	if body := fx.sheet(t); !strings.Contains(body, `class="pill pill--api"`) {
		t.Errorf("the sheet should mark an API-written plan")
	}
	// The trader saving the sheet makes it theirs.
	if code, out := fx.savePlan(t, url.Values{"stop": {"7241"}, "risk": {"1500.00"}, "target": {"7271"}, "thesis": {"api plan"}}); code != http.StatusOK || strings.Contains(out, `class="pill pill--api"`) {
		t.Errorf("after the trader's save the plan still reads as api (%d)", code)
	}

	if code, out := post(acct, "ESM6", `{"stop": -1}`, fx.ctx); code != http.StatusBadRequest {
		t.Errorf("negative stop = %d %v", code, out)
	}
	if code, _ := post(acct, "NQM6", `{"stop": 1}`, fx.ctx); code != http.StatusConflict {
		t.Errorf("flat contract = %d, want 409", code)
	}
	if code, _ := post("999999", "ESM6", `{"stop": 1}`, fx.ctx); code != http.StatusNotFound {
		t.Errorf("unknown account = %d, want 404", code)
	}
}
